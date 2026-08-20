package experiment

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chaos-injector/internal/fault"
)

// fakeFault records its lifecycle for scheduler tests.
type fakeFault struct {
	injected  bool
	recovered bool
}

func (f *fakeFault) Name() string        { return "fake" }
func (f *fakeFault) Description() string { return "fake fault for tests" }
func (f *fakeFault) Check() error        { return nil }
func (f *fakeFault) Inject() error       { f.injected = true; return nil }
func (f *fakeFault) Recover() error      { f.recovered = true; return nil }
func (f *fakeFault) Describe() string    { return "fake()" }

type failingFault struct{ fakeFault }

func (f *failingFault) Check() error { return fault.Errf("boom") }

func phases(exp *Experiment) []string {
	var out []string
	for _, ev := range exp.Timeline() {
		out = append(out, ev.Phase)
	}
	return out
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func count(list []string, want string) int {
	n := 0
	for _, s := range list {
		if s == want {
			n++
		}
	}
	return n
}

func TestAutoRecoversAfterDuration(t *testing.T) {
	f := &fakeFault{}
	exp := &Experiment{Name: "t", Fault: f, Duration: 300 * time.Millisecond}
	if err := exp.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !f.injected || !f.recovered {
		t.Fatalf("injected=%v recovered=%v", f.injected, f.recovered)
	}
	got := phases(exp)
	for _, want := range []string{"start", "check", "inject", "armed", "recover", "auto", "done"} {
		if !contains(got, want) {
			t.Fatalf("missing phase %q in %v", want, got)
		}
	}
}

func TestRecoverIsIdempotent(t *testing.T) {
	f := &fakeFault{}
	exp := &Experiment{Name: "t", Fault: f, Duration: time.Minute}
	if err := exp.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	exp.Recover()
	exp.Recover() // second call must be a no-op
	if !f.recovered {
		t.Fatal("fault not recovered")
	}
	if n := count(phases(exp), "recover"); n != 1 {
		t.Fatalf("recover recorded %d times, want 1", n)
	}
}

func TestCheckFailureLeavesNothingBehind(t *testing.T) {
	exp := &Experiment{Name: "t", Fault: &failingFault{}, Duration: time.Minute}
	if err := exp.Run(); err == nil {
		t.Fatal("expected error from failing fault")
	}
	got := phases(exp)
	if !contains(got, "error") {
		t.Fatalf("missing error phase in %v", got)
	}
	if contains(got, "inject") || contains(got, "recover") {
		t.Fatalf("inject/recover must not happen after failed check: %v", got)
	}
}

func TestTimelineWrittenToNestedDir(t *testing.T) {
	f := &fakeFault{}
	exp := &Experiment{Name: "t", Fault: f, Duration: time.Minute}
	if err := exp.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	exp.Recover()

	target := filepath.Join(t.TempDir(), "nested", "dir", "exp.json")
	if err := exp.WriteTimeline(target); err != nil {
		t.Fatalf("WriteTimeline: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var payload struct {
		Experiment string `json:"experiment"`
		Recovered  bool   `json:"recovered"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if payload.Experiment != "t" || !payload.Recovered {
		t.Fatalf("unexpected payload: %+v", payload)
	}
}

// TestTimelineRecordsSeedAndSnapshot verifies the reproducibility evidence:
// the random-mode seed and the before/after system snapshots must be part
// of the persisted timeline.
func TestTimelineRecordsSeedAndSnapshot(t *testing.T) {
	exp := &Experiment{Name: "random-cpu", Fault: &fakeFault{}, Duration: time.Minute, Seed: 42, SeedSet: true}
	if err := exp.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	exp.Recover()

	target := filepath.Join(t.TempDir(), "exp.json")
	if err := exp.WriteTimeline(target); err != nil {
		t.Fatalf("WriteTimeline: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var payload struct {
		Seed     int64 `json:"seed"`
		Snapshot struct {
			Before struct {
				Hostname string `json:"hostname"`
				Platform string `json:"platform"`
			} `json:"before"`
			After struct {
				Hostname string `json:"hostname"`
				Platform string `json:"platform"`
			} `json:"after"`
		} `json:"snapshot"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if payload.Seed != 42 {
		t.Fatalf("seed = %d, want 42", payload.Seed)
	}
	if payload.Snapshot.Before.Hostname == "" || payload.Snapshot.After.Hostname == "" {
		t.Fatalf("snapshot hostname missing: %+v", payload.Snapshot)
	}
	if payload.Snapshot.Before.Platform == "" || payload.Snapshot.After.Platform == "" {
		t.Fatalf("snapshot platform missing: %+v", payload.Snapshot)
	}
}

// TestTimelineOmitsSeedWhenUnset verifies that non-random experiments do not
// carry a seed field.
func TestTimelineOmitsSeedWhenUnset(t *testing.T) {
	exp := &Experiment{Name: "cpu", Fault: &fakeFault{}, Duration: time.Minute}
	if err := exp.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	exp.Recover()

	target := filepath.Join(t.TempDir(), "exp.json")
	if err := exp.WriteTimeline(target); err != nil {
		t.Fatalf("WriteTimeline: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(data), `"seed"`) {
		t.Fatalf("timeline must not contain a seed field: %s", data)
	}
}
