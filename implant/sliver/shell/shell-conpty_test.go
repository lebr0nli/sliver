//go:build windows

package shell

import (
	"bytes"
	"math"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestConPTYSize(t *testing.T) {
	tests := []struct {
		name string
		rows uint32
		cols uint32
		want windows.Coord
	}{
		{"reported geometry", 40, 120, windows.Coord{X: 120, Y: 40}},
		{"unreported geometry", 0, 0, windows.Coord{X: defaultConPTYCols, Y: defaultConPTYRows}},
		{"geometry beyond int16", 1 << 20, 1 << 20, windows.Coord{X: math.MaxInt16, Y: math.MaxInt16}},
	}
	for _, test := range tests {
		if got := conPTYSize(test.rows, test.cols); got != test.want {
			t.Errorf("%s: conPTYSize(%d, %d) = %+v, want %+v", test.name, test.rows, test.cols, got, test.want)
		}
	}
}

// TestConPTYShellRoundTrip drives the whole pseudoconsole lifecycle the way the
// shell tunnel handler does: allocate, run a command, resize, then drain the
// output to EOF before reaping. Draining before reaping is the ordering that
// deadlocks if the pseudoconsole is only released from Wait.
func TestConPTYShellRoundTrip(t *testing.T) {
	if err := conPTYSupported(); err != nil {
		t.Skipf("host has no ConPTY support: %v", err)
	}

	systemShell, err := StartInteractive(1, commandPrompt, true, 24, 80)
	if err != nil {
		t.Fatalf("start ConPTY shell: %v", err)
	}
	if !systemShell.EnablePTY {
		t.Fatal("ConPTY shell fell back to a piped shell")
	}
	if systemShell.Pid == 0 {
		t.Fatal("ConPTY shell reported no pid")
	}
	session := NewSession(systemShell)
	t.Cleanup(session.Stop)

	// %OS% is expanded by the shell, so the marker can only appear in command
	// output and never in the input the pseudoconsole echoes back.
	const marker = "Windows_NT-conpty-ok"
	var (
		once     sync.Once
		executed = make(chan struct{})
		drained  = make(chan []byte, 1)
	)
	go func() {
		var output bytes.Buffer
		buf := make([]byte, 4096)
		for {
			n, err := systemShell.Stdout.Read(buf)
			if n > 0 {
				output.Write(buf[:n])
				if bytes.Contains(output.Bytes(), []byte(marker)) {
					once.Do(func() { close(executed) })
				}
			}
			if err != nil {
				drained <- output.Bytes()
				return
			}
		}
	}()

	if _, err := systemShell.Stdin.Write([]byte("echo %OS%-conpty-ok\r\n")); err != nil {
		t.Fatalf("write to ConPTY shell: %v", err)
	}
	select {
	case <-executed:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the ConPTY shell to run a command")
	}

	// The shell resize handler reaches the pseudoconsole through this exact
	// assertion on the tunnel's writer.
	resizer, ok := systemShell.Stdin.(interface{ Resize(rows, cols uint32) error })
	if !ok {
		t.Fatal("ConPTY shell input does not support resizing")
	}
	if err := resizer.Resize(40, 120); err != nil {
		t.Fatalf("resize pseudoconsole: %v", err)
	}

	session.Stop()
	select {
	case <-drained:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out draining ConPTY output")
	}

	reaped := make(chan error, 1)
	go func() { reaped <- systemShell.Wait() }()
	select {
	case <-reaped:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out reaping the ConPTY shell")
	}
}
