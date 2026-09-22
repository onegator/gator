package process

import (
	"testing"
	"time"
)

// A process override is written the way the templates are documented: timeouts as "24h" and a
// rollback ceiling as max_rollbacks. JSON used to refuse the first and silently drop the second.
func TestAProjectTemplateReadsTheWayItIsWritten(t *testing.T) {
	c, err := ParseProjectConfig([]byte(`{"templates":{"chore":{"kind":"chore","max_rollbacks":3,
		"phases":[{"name":"implementation","owner":"runner","role":"worker","gate":"both","timeout":"24h"},
		          {"name":"approved","owner":"human","gate":"auto","timeout":0}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	tp := c.Templates["chore"]
	if tp.MaxRollbacks != 3 {
		t.Errorf("max_rollbacks = %d", tp.MaxRollbacks)
	}
	if got := tp.Phases[0].Timeout.Std(); got != 24*time.Hour {
		t.Errorf("timeout = %v", got)
	}
	if tp.Phases[1].Timeout.Std() != 0 {
		t.Errorf("a zero timeout should stay zero")
	}
	if _, err := ParseProjectConfig([]byte(`{"templates":{"chore":{"phases":[{"name":"x","timeout":"soon"}]}}}`)); err == nil {
		t.Error("a timeout that is not a duration should be refused")
	}
}

func TestTimeoutsAreWrittenTheWayAPersonWritesThem(t *testing.T) {
	for d, want := range map[time.Duration]string{0: "0", 24 * time.Hour: "24h", 90 * time.Minute: "1h30m", 720 * time.Hour: "720h", 45 * time.Second: "45s"} {
		if got := timeoutText(Duration(d)); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}
