package api

import (
	"encoding/json"
	"errors"
	"reflect"
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
			Knobs{AutoStop: "off", AutoStart: def.AutoStart, SoftLimit: def.SoftLimit, IdleTimeout: def.IdleTimeout},
		},
		{
			"only soft_limit", `{"soft_limit":5}`,
			Knobs{AutoStop: def.AutoStop, AutoStart: def.AutoStart, SoftLimit: 5, IdleTimeout: def.IdleTimeout},
		},
		{
			"only min_machines_running", `{"min_machines_running":2}`,
			Knobs{AutoStop: def.AutoStop, AutoStart: def.AutoStart,
				MinMachinesRunning: 2, SoftLimit: def.SoftLimit, IdleTimeout: def.IdleTimeout},
		},
		{
			"only idle_timeout", `{"idle_timeout":1800}`,
			Knobs{AutoStop: def.AutoStop, AutoStart: def.AutoStart, SoftLimit: def.SoftLimit, IdleTimeout: 1800},
		},
		{
			"empty object keeps every default", `{}`, def,
		},
		{
			// An explicit false must still win: merging cannot mean ignoring.
			"explicit auto_start false", `{"auto_start":false}`,
			Knobs{AutoStop: def.AutoStop, AutoStart: false, SoftLimit: def.SoftLimit, IdleTimeout: def.IdleTimeout},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeKnobs(json.RawMessage(tc.raw))
			if err != nil {
				t.Fatalf("DecodeKnobs: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
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
	if !reflect.DeepEqual(got, DefaultKnobs()) {
		t.Errorf("got %+v, want the defaults", got)
	}
}

// A schedule is a cron the matcher accepts and exactly one target; the whole
// list is bounded. Each refusal names the entry so a compose file or a
// package.json can be fixed in one pass.
func TestDecodeKnobsValidatesSchedules(t *testing.T) {
	good := `{"schedules":[{"cron":"0 5 * * *","path":"/jobs/digest"},{"cron":"@hourly","cmd":"./tick"}]}`
	k, err := DecodeKnobs(json.RawMessage(good))
	if err != nil {
		t.Fatalf("a valid schedule list was refused: %v", err)
	}
	if len(k.Schedules) != 2 || k.Schedules[0].Path != "/jobs/digest" || k.Schedules[1].Cmd != "./tick" {
		t.Errorf("schedules = %+v", k.Schedules)
	}
	if k.AutoStart != true || k.IdleTimeout != DefaultIdleTimeoutSeconds {
		t.Errorf("a schedules-only object zeroed its neighbours: %+v", k)
	}

	for _, tc := range []struct{ raw, want string }{
		{`{"schedules":[{"cron":"0 5 * * *"}]}`, "needs a path"},
		{`{"schedules":[{"cron":"0 5 * * *","path":"/a","cmd":"b"}]}`, "both"},
		{`{"schedules":[{"cron":"0 5 * * *","path":"jobs"}]}`, "must start with /"},
		{`{"schedules":[{"cron":"every day","path":"/a"}]}`, "5 fields"},
		{`{"schedules":[{"cron":"0 5 * * *","path":"/a"},{"cron":"bad","cmd":"x"}]}`, "schedules[1]"},
	} {
		_, err := DecodeKnobs(json.RawMessage(tc.raw))
		if err == nil || !errors.Is(err, ErrInvalidKnobs) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want ErrInvalidKnobs mentioning %q", tc.raw, err, tc.want)
		}
	}

	many := `{"schedules":[` + strings.Repeat(`{"cron":"* * * * *","path":"/x"},`, MaxSchedules) + `{"cron":"* * * * *","path":"/x"}]}`
	if _, err := DecodeKnobs(json.RawMessage(many)); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("%d schedules were accepted: %v", MaxSchedules+1, err)
	}

	// An explicit empty list is a valid way to say "none", and it is not the
	// same value as absent: the deploy merge relies on the difference.
	k, err = DecodeKnobs(json.RawMessage(`{"schedules":[]}`))
	if err != nil || k.Schedules == nil || len(k.Schedules) != 0 {
		t.Errorf("schedules: [] should decode to an empty, non-nil list: %+v, %v", k.Schedules, err)
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

// The timer is bounded on both sides: zero would suspend a machine the moment
// it went quiet, and more than an hour is the bill a forgotten value is not
// allowed to run up.
func TestDecodeKnobsBoundsTheIdleTimeout(t *testing.T) {
	for _, raw := range []string{`{"idle_timeout":0}`, `{"idle_timeout":-5}`, `{"idle_timeout":3601}`} {
		if _, err := DecodeKnobs(json.RawMessage(raw)); err == nil || !errors.Is(err, ErrInvalidKnobs) {
			t.Errorf("%s: err = %v, want ErrInvalidKnobs", raw, err)
		}
	}
	for _, raw := range []string{`{"idle_timeout":1}`, `{"idle_timeout":3600}`} {
		if _, err := DecodeKnobs(json.RawMessage(raw)); err != nil {
			t.Errorf("%s: %v, want the bound itself to be accepted", raw, err)
		}
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
