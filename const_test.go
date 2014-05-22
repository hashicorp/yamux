package yamux

import (
	"testing"
)

func TestConst(t *testing.T) {
	if protoVersion != 0 {
		t.Fatalf("bad: %v", protoVersion)
	}

	if typeData != 0 {
		t.Fatalf("bad: %v", typeData)
	}
	if typeWindowUpdate != 1 {
		t.Fatalf("bad: %v", typeWindowUpdate)
	}
	if typePing != 2 {
		t.Fatalf("bad: %v", typePing)
	}
	if typeGoAway != 3 {
		t.Fatalf("bad: %v", typeGoAway)
	}

	if flagSYN != 1 {
		t.Fatalf("bad: %v", flagSYN)
	}
	if flagACK != 2 {
		t.Fatalf("bad: %v", flagACK)
	}
	if flagFIN != 4 {
		t.Fatalf("bad: %v", flagFIN)
	}
	if flagRST != 8 {
		t.Fatalf("bad: %v", flagRST)
	}
	if flagLZW != 16 {
		t.Fatalf("bad: %v", flagLZW)
	}

	if goAwayNormal != 0 {
		t.Fatalf("bad: %v", goAwayNormal)
	}
	if goAwayProtoErr != 1 {
		t.Fatalf("bad: %v", goAwayProtoErr)
	}
	if goAwayInternalErr != 2 {
		t.Fatalf("bad: %v", goAwayInternalErr)
	}
}
