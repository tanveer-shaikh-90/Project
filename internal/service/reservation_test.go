package service

import (
	"testing"
)

func TestValidateSeats(t *testing.T) {
	for _, seats := range [][]string{nil, {""}, {" A1"}, {"A1", "A1"}, {"A\x00"}} {
		if _, err := validateSeats(seats, 100); err == nil {
			t.Fatalf("accepted invalid seats: %q", seats)
		}
	}
	seats, err := validateSeats([]string{"B1", "A1"}, 100)
	if err != nil || seats[0] != "A1" {
		t.Fatalf("not canonical: %v %v", seats, err)
	}
	if hashRequest("show", []string{"A,B", "C"}) == hashRequest("show", []string{"A", "B,C"}) {
		t.Fatal("ambiguous request encoding")
	}
}