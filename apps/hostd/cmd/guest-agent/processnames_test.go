package main

import (
	"reflect"
	"testing"
)

// A registered process that reuses a name the boot's own set already has is
// dropped from the replay, instead of making orderByNeeds refuse the whole set
// and start nothing.
func TestASavedProcessCannotShadowADeclaredOne(t *testing.T) {
	taken := []processSpec{{Name: DefaultProcess, Cmd: "serve"}, {Name: "worker", Cmd: "work"}}
	saved := []processSpec{{Name: "worker", Cmd: "other"}, {Name: DefaultProcess, Cmd: "x"},
		{Name: "cron", Cmd: "tick"}, {Name: "cron", Cmd: "tick2"}}

	got := withoutNamesIn(saved, taken)
	want := []processSpec{{Name: "cron", Cmd: "tick"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("replayed %v, want only %v", got, want)
	}
	if _, err := orderByNeeds(append(taken, got...)); err != nil {
		t.Errorf("the boot set still does not order: %v", err)
	}
}

// Registration refuses the image's own process name and any name the machine
// already knows, stopped or not -- the stopped case is the one that slipped
// through when only a RUNNING name was refused.
func TestRegisterRefusesATakenName(t *testing.T) {
	s := &supervisor{procs: map[string]*process{"worker": {spec: processSpec{Name: "worker"}}}}
	if err := s.register(processSpec{Name: DefaultProcess, Cmd: "x"}); err == nil {
		t.Error("registering a process named app was accepted")
	}
	if err := s.register(processSpec{Name: "worker", Cmd: "x"}); err == nil {
		t.Error("registering over a stopped process of the same name was accepted")
	}
}
