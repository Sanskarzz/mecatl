package keymap

import "testing"

func TestParseUnknownAction(t *testing.T) {
	_, err := Parse(map[string][]string{"Bogus": {"ctrl+a"}})
	if err == nil {
		t.Fatalf("expected error for unknown action")
	}
}

func TestParseEmptyChord(t *testing.T) {
	_, err := Parse(map[string][]string{"Agents": {""}})
	if err == nil {
		t.Fatalf("expected error for empty chord")
	}
}

func TestValidateBareRuneOnGlobal(t *testing.T) {
	res, err := Parse(map[string][]string{"Agents": {"a"}})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := Validate(res); err == nil {
		t.Fatalf("expected bare rune rejection")
	}
}

func TestValidateOverlayCollision(t *testing.T) {
	res, err := Parse(map[string][]string{
		"Refresh": {"r"},
		"Tasks":   {"r"},
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := Validate(res); err == nil {
		t.Fatalf("expected overlay collision rejection")
	}
}

func TestValidateGlobalCollision(t *testing.T) {
	res, err := Parse(map[string][]string{
		"Agents":     {"ctrl+a"},
		"ExpandTools": {"ctrl+a"},
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := Validate(res); err == nil {
		t.Fatalf("expected global collision rejection")
	}
}

func TestValidateApprovalConsistency(t *testing.T) {
	res, err := Parse(map[string][]string{
		"Deny":   {"enter"},
		"Submit": {"enter"},
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := Validate(res); err == nil {
		t.Fatalf("expected approval collision rejection")
	}
}

func TestValidateSubmitNewlineDistinct(t *testing.T) {
	res, err := Parse(map[string][]string{
		"Submit":  {"enter"},
		"Newline": {"enter"},
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := Validate(res); err == nil {
		t.Fatalf("expected submit/newline collision rejection")
	}
}

func TestValidAgentsOverride(t *testing.T) {
	res, err := Parse(map[string][]string{"Agents": {"ctrl+\\"}})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := Validate(res); err != nil {
		t.Fatalf("validate: %v", err)
	}
}
