package cheesedb

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A second Open of the same folder, while the first is open, is refused;
// after Close the folder opens again.
func TestLockSameProcess(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "s")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open: got %v, want ErrLocked", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	s2.Close()
}

// The lock file is not mistaken for an agent, and a failed Open (here, a
// damaged key) does not leave the folder locked.
func TestLockFileIgnoredAndReleased(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "s")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s.CreateAgent("alice")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	keyPath := filepath.Join(dir, "616c696365", keyFile)
	good, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open with a damaged key: got %v, want ErrCorrupt", err)
	}
	if err := os.WriteFile(keyPath, good, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir) // would be ErrLocked if the failed Open kept the lock
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.OpenAgent(tok); err != nil {
		t.Fatal(err)
	}
}

// TestLockOtherProcess runs a real second process (this test binary again,
// in TestLockHelper) that opens the store and holds it. This process must be
// refused. Then the helper is killed, as a crash would end it, and the
// folder must open: the OS released the lock, with nothing to clean up.
func TestLockOtherProcess(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "s")
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockHelper$")
	cmd.Env = append(os.Environ(), "CHEESEDB_LOCK_HELPER="+dir)
	stdin, err := cmd.StdinPipe() // kept open: the helper waits on it
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || line != "locked\n" {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("helper said %q, %v", line, err)
	}

	if _, err := Open(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("Open while another process holds it: got %v, want ErrLocked", err)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open after the other process was killed: %v", err)
	}
	s.Close()
}

// TestLockHelper is the second process for TestLockOtherProcess; run on its
// own, it does nothing.
func TestLockHelper(t *testing.T) {
	dir := os.Getenv("CHEESEDB_LOCK_HELPER")
	if dir == "" {
		t.Skip("helper process for TestLockOtherProcess")
	}
	if _, err := Open(dir); err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	fmt.Println("locked")
	io.Copy(io.Discard, os.Stdin) // hold the store until killed
	os.Exit(0)
}
