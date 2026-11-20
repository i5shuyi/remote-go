package backend

import "testing"

func TestVerificationModeSurvivesFailedSave(t *testing.T) {
	a := New()
	a.cfgPath = t.TempDir() // A directory cannot be replaced by the config file.
	const original = "old-verification-code"
	a.password = original
	a.cfg.FixedPassword = original
	for _, next := range []string{"", "new-verification-code"} {
		if err := a.SetFixedPassword(next); err == nil {
			t.Fatal("expected persistence failure")
		}
		if a.password != original || a.cfg.FixedPassword != original {
			t.Fatal("failed change replaced the current verification code")
		}
	}
}
