package skip

import (
	"testing"

	"her/tools"
)

func TestHandle_SetsDoneCalled(t *testing.T) {
	ctx := &tools.Context{}

	result := Handle(`{"reason": "nothing new"}`, ctx)

	if !ctx.DoneCalled {
		t.Error("skip should set DoneCalled = true")
	}
	if result != "skipped — turn complete" {
		t.Errorf("unexpected result: %q", result)
	}
}

func TestHandle_EmptyReason(t *testing.T) {
	ctx := &tools.Context{}

	result := Handle(`{}`, ctx)

	if !ctx.DoneCalled {
		t.Error("skip with empty args should still set DoneCalled")
	}
	if result != "skipped — turn complete" {
		t.Errorf("unexpected result: %q", result)
	}
}

func TestHandle_MalformedJSON(t *testing.T) {
	ctx := &tools.Context{}

	result := Handle(`not json`, ctx)

	if !ctx.DoneCalled {
		t.Error("skip with bad JSON should still set DoneCalled (reason is optional)")
	}
	if result != "skipped — turn complete" {
		t.Errorf("unexpected result: %q", result)
	}
}
