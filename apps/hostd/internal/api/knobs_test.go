package api

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// A caller who sets one field must not have every other field zeroed.
//
// Assigning a decoded struct wholesale turned {"auto_stop":"suspend"} into
// auto_start:false, so the machine suspended after a minute and the router then
// refused to wake it -- a permanently dead URL earned by setting one unrelated
// field.
func TestDecodeKnobsMergesOntoDefaults(t *testing.T) {
	def := DefaultKnobs()

	for _, tc := range []struct {
		name string
		raw  string
		want Knobs
	}{
		{
			"only auto_stop", `{"auto_stop":"off"}`,
			Knobs{AutoStop: "off", AutoStart: def.AutoStart, SoftLimit: def.SoftLimit},
		},
		{
			"only soft_limit", `{"soft_limit":5}`,
			Knobs{AutoStop: def.AutoStop, AutoStart: def.AutoStart, SoftLimit: 5},
		},
		{
			"only min_machines_running", `{"min_machines_running":2}`,
			Knobs{AutoStop: def.AutoStop, AutoStart: def.AutoStart,
				MinMachinesRunning: 2, SoftLimit: def.SoftLimit},
		},
		{
			"empty object keeps every default", `{}`, def,
		},
		{
			// An explicit false must still win: merging cannot mean ignoring.
			"explicit auto_start false", `{"auto_start":false}`,
			Knobs{AutoStop: def.AutoStop, AutoStart: false, SoftLimit: def.SoftLimit},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeKnobs(json.RawMessage(tc.raw))
			if err != nil {
				t.Fatalf("DecodeKnobs: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestDecodeKnobsEmptyInputIsDefaults(t *testing.T) {
	got, err := DecodeKnobs(nil)
	if err != nil {
		t.Fatalf("DecodeKnobs(nil): %v", err)
	}
	if got != DefaultKnobs() {
		t.Errorf("got %+v, want the defaults", got)
	}
}

func TestDecodeKnobsRejectsMalformed(t *testing.T) {
	if _, err := DecodeKnobs(json.RawMessage(`{"auto_stop":`)); err == nil {
		t.Error("malformed knobs were accepted")
	}
}

// auto_stop names a policy the idle monitor acts on, so a value it does not
// act on is refused rather than stored. "stop" in particular was accepted and
// silently behaved as suspend.
func TestDecodeKnobsRefusesAnAutoStopThatDoesNotExist(t *testing.T) {
	for _, raw := range []string{`{"auto_stop":"stop"}`, `{"auto_stop":"sometimes"}`} {
		_, err := DecodeKnobs(json.RawMessage(raw))
		if err == nil {
			t.Errorf("%s was accepted", raw)
			continue
		}
		if !errors.Is(err, ErrInvalidKnobs) {
			t.Errorf("%s: err = %v, want one wrapping ErrInvalidKnobs so the API answers 400", raw, err)
		}
	}
	if _, err := DecodeKnobs(json.RawMessage(`{"auto_stop":"stop"}`)); err == nil || !strings.Contains(err.Error(), "suspend") {
		t.Errorf("refusing stop should name suspend as the alternative, got %v", err)
	}
}

// Defaults must keep a machine reachable: auto_start false would strand it.
func TestDefaultKnobsAreReachable(t *testing.T) {
	k := DefaultKnobs()
	if !k.AutoStart {
		t.Error("the default policy does not auto-start; a suspended machine would never wake")
	}
	if k.SoftLimit <= 0 {
		t.Error("the default soft limit must be positive")
	}
}
