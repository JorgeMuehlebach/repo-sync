//go:build darwin

package background

import "testing"

type completedProgram struct {
	done       chan struct{}
	startCalls int
	stopCalls  int
}

func (p *completedProgram) Start() error {
	p.startCalls++
	close(p.done)
	return nil
}

func (p *completedProgram) Stop() error {
	p.stopCalls++
	return nil
}

func (p *completedProgram) Done() <-chan struct{} { return p.done }

func TestRunReturnsWhenProgramCompletes(t *testing.T) {
	program := &completedProgram{done: make(chan struct{})}
	controller := &controller{program: program}
	if err := controller.Run(); err != nil {
		t.Fatal(err)
	}
	if program.startCalls != 1 || program.stopCalls != 1 {
		t.Fatalf("Start()/Stop() calls = %d/%d, want 1/1", program.startCalls, program.stopCalls)
	}
}
