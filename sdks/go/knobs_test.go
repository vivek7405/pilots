package pilots

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// What a request's knobs must do on the wire.
//
// hostd merges them onto a value it already seeded (api.DecodeKnobs), so a
// field the caller never mentioned has to be ABSENT from the JSON, not
// present as a zero. A *Knobs cannot do that -- Knobs carries no omitempty --
// and the failure is not cosmetic: an unmentioned auto_start arrives as false,
// the replica suspends after a minute, and the router then refuses to wake it.
// The URL is dead, from a call that only raised a concurrency limit.
func TestAPartialPatchSendsOnlyWhatWasSet(t *testing.T) {
	got := marshal(t, CreateMachineRequest{
		Image: "img_1",
		Knobs: &KnobsPatch{SoftLimit: Ptr(50)},
	})

	knobs, ok := got["knobs"].(map[string]any)
	if !ok {
		t.Fatalf("knobs is %T, want an object", got["knobs"])
	}
	if want := map[string]any{"soft_limit": float64(50)}; !reflect.DeepEqual(knobs, want) {
		t.Errorf("sent %v, want only the field that was set: %v", knobs, want)
	}
	// Named individually, because each is its own way to break a machine:
	// auto_start false is a URL that never wakes, min_machines_running 0
	// silently removes a warm floor, auto_stop "" is not a policy at all.
	for _, absent := range []string{"auto_start", "min_machines_running", "auto_stop"} {
		if _, present := knobs[absent]; present {
			t.Errorf("%s was sent by a caller who never mentioned it", absent)
		}
	}
}

// The other half: a zero the caller MEANT must survive. This is why the
// fields are pointers rather than plain values with omitempty -- omitempty
// cannot tell "scale to zero" from "unset", and both of these are policies
// somebody deliberately asked for.
func TestADeliberateZeroIsStillSent(t *testing.T) {
	got := marshal(t, DeployRequest{
		Build: "b_1",
		Knobs: &KnobsPatch{MinMachinesRunning: Ptr(0), AutoStart: Ptr(false)},
	})

	knobs := got["knobs"].(map[string]any)
	if v, ok := knobs["min_machines_running"]; !ok || v != float64(0) {
		t.Errorf("min_machines_running = %v (present %v), want an explicit 0", v, ok)
	}
	if v, ok := knobs["auto_start"]; !ok || v != false {
		t.Errorf("auto_start = %v (present %v), want an explicit false", v, ok)
	}
}

// A patch with nothing set sends no knobs at all, so hostd keeps every
// inherited value -- the same outcome as omitting the field.
func TestAnEmptyPatchSendsAnEmptyObject(t *testing.T) {
	got := marshal(t, DeployRequest{Build: "b_1", Knobs: &KnobsPatch{}})
	knobs := got["knobs"].(map[string]any)
	if len(knobs) != 0 {
		t.Errorf("an empty patch sent %v, want {}", knobs)
	}

	got = marshal(t, DeployRequest{Build: "b_1"})
	if _, present := got["knobs"]; present {
		t.Error("a request with no knobs still carried the key")
	}
}

// Every request that carries knobs carries the patch, not the full policy.
// Missing one leaves the trap open on exactly that call, which is how the
// pattern got here: the shape was right on the machine create and wrong on
// the deploy that turned out to be load-bearing.
func TestEveryRequestCarriesThePatch(t *testing.T) {
	for _, req := range []any{
		CreateMachineRequest{}, CreateServiceRequest{}, DeployRequest{}, ComposeStep{},
	} {
		rt := reflect.TypeOf(req)
		field, ok := rt.FieldByName("Knobs")
		if !ok {
			continue
		}
		if want := reflect.TypeOf(&KnobsPatch{}); field.Type != want {
			t.Errorf("%s.Knobs is %v, want %v -- a request merges, so it patches",
				rt.Name(), field.Type, want)
		}
	}
}

// KnobsPatch is not in wireTypes, so the drift test does not hold it to
// hostd. This does: a knob added to Knobs (which drift DOES check against
// hostd) has to be sayable in a patch, or it becomes a field no caller can
// set without zeroing its neighbours.
func TestKnobsPatchCoversEveryKnob(t *testing.T) {
	tags := func(v any) []string {
		rt := reflect.TypeOf(v)
		var out []string
		for i := range rt.NumField() {
			name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
			if name != "" && name != "-" {
				out = append(out, name)
			}
		}
		slices.Sort(out)
		return out
	}
	full, patch := tags(Knobs{}), tags(KnobsPatch{})
	if !slices.Equal(full, patch) {
		t.Errorf("KnobsPatch has %v, Knobs has %v", patch, full)
	}

	// And every one of them optional, or the patch is a policy again.
	rt := reflect.TypeOf(KnobsPatch{})
	for i := range rt.NumField() {
		f := rt.Field(i)
		if f.Type.Kind() != reflect.Pointer {
			t.Errorf("KnobsPatch.%s is %v, want a pointer so absent and zero differ",
				f.Name, f.Type)
		}
		if !strings.Contains(f.Tag.Get("json"), ",omitempty") {
			t.Errorf("KnobsPatch.%s has no omitempty, so nil would be sent as null", f.Name)
		}
	}
}

func marshal(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}
