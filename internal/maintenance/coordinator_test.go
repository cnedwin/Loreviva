package maintenance

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestMaintenanceConcurrentReadersDrainAndRetireGeneration(t *testing.T) {
	ctx := context.Background()
	c, e := CreateCoordinator(t.TempDir(), "installation", "database", "files")
	if e != nil {
		t.Fatal(e)
	}
	p, e := c.Register(ctx, "api", []Writer{APIWriter, StoreWriter})
	if e != nil {
		t.Fatal(e)
	}
	_, done1, e := p.Enter(ctx, APIWriter)
	if e != nil {
		t.Fatal(e)
	}
	short, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, done2, e := p.Enter(short, StoreWriter)
	if e != nil {
		done1()
		t.Fatal("ordinary operations serialized", e)
	}
	if e = c.BeginClose(ctx, "maintenance"); e != nil {
		t.Fatal(e)
	}
	if _, release, e := p.Enter(short, APIWriter); e == nil {
		release()
		t.Fatal("new ordinary operation admitted while closing")
	}
	blocked, cancelBlocked := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancelBlocked()
	if e = c.Drain(blocked, "maintenance"); e == nil {
		t.Fatal("drain ignored two outstanding OS permits")
	}
	done1()
	done2()
	if e = c.Drain(ctx, "maintenance"); e != nil {
		t.Fatal(e)
	}
	reopened, e := OpenCoordinator(c.Directory(), "installation", "database", "files")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = reopened.Register(ctx, "late-api", []Writer{APIWriter}); e == nil {
		t.Fatal("closed maintenance reopened after restart")
	}
	if _, release, e := p.Enter(ctx, StoreWriter); e == nil {
		release()
		t.Fatal("retired generation wrote")
	}
}

func TestMaintenanceTwoProcessOSDrain(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c, e := CreateCoordinator(dir, "installation", "database", "files")
	if e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMaintenanceProcessHelper$")
	cmd.Env = append(os.Environ(), "LOREVIVA_COORD_HELPER="+dir)
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	stdin, e := cmd.StdinPipe()
	if e != nil {
		t.Fatal(e)
	}
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() {
		stdin.Close()
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	reader := bufio.NewReader(stdout)
	line, e := reader.ReadString('\n')
	if e != nil || strings.TrimSpace(line) != "ADMITTED" {
		t.Fatal("child failed to hold OS shared permit", line, e)
	}
	if e = c.BeginClose(ctx, "plan"); e != nil {
		t.Fatal(e)
	}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if e = c.Drain(short, "plan"); e == nil {
		t.Fatal("cross-process shared operation ignored")
	}
	if _, e = io.WriteString(stdin, "release\n"); e != nil {
		t.Fatal(e)
	}
	if e = cmd.Wait(); e != nil {
		t.Fatal(e)
	}
	if e = c.Drain(ctx, "plan"); e != nil {
		t.Fatal(e)
	}
}
func TestMaintenanceProcessHelper(t *testing.T) {
	dir := os.Getenv("LOREVIVA_COORD_HELPER")
	if dir == "" {
		t.Skip("child process only")
	}
	c, e := OpenCoordinator(dir, "installation", "database", "files")
	if e != nil {
		t.Fatal(e)
	}
	p, e := c.Register(context.Background(), "child-worker", []Writer{StoreWriter})
	if e != nil {
		t.Fatal(e)
	}
	_, done, e := p.Enter(context.Background(), StoreWriter)
	if e != nil {
		t.Fatal(e)
	}
	fmt.Println("ADMITTED")
	if _, e = bufio.NewReader(os.Stdin).ReadString('\n'); e != nil {
		t.Fatal(e)
	}
	done()
}
