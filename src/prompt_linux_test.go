package main

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestHelperAskSecret(t *testing.T) {
	if os.Getenv("TEST_ASK_SECRET") != "1" {
		t.Skip()
	}
	if _, err := newTerminalPrompter().askSecret("Password"); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skip(err)
	}
	fd := int(master.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err = os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	return master, slave
}

func echoOn(t *testing.T, f *os.File) bool {
	t.Helper()
	tio, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	return tio.Lflag&unix.ECHO != 0
}

func TestCtrlCAtThePasswordPromptRestoresEchoAndExits130(t *testing.T) {
	master, slave := openPTY(t)
	defer master.Close()
	defer slave.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperAskSecret$")
	cmd.Env = append(os.Environ(), "TEST_ASK_SECRET=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var out []byte
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := master.Read(buf)
			mu.Lock()
			out = append(out, buf[:n]...)
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	seen := func(text string) bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(string(out), text)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !seen("Password: ") {
		if time.Now().After(deadline) {
			mu.Lock()
			t.Fatalf("no password prompt, got %q", out)
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if echoOn(t, slave) {
		t.Fatal("the terminal still echoes at the password prompt")
	}

	if _, err := master.Write([]byte{3}); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 130 {
			t.Errorf("Ctrl-C ended the login with %v, want exit code 130", err)
		}
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		t.Fatal("the login did not end after Ctrl-C")
	}
	if !echoOn(t, slave) {
		t.Error("the terminal does not echo after Ctrl-C at the password prompt")
	}
}
