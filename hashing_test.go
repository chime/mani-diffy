package main

import (
	"os"
	"testing"
)

func TestNewJSONHashStore(t *testing.T) {
	f, err := os.CreateTemp("", "")
	if err != nil {
		t.Fatal(err)
	}

	h, err := NewJSONHashStore(f.Name(), HashStrategyReadWrite)
	if err != nil {
		t.Fatal(err)
	}

	if err := h.Add("foo", "bar"); err != nil {
		t.Fatal(err)
	}

	if err := h.Save(); err != nil {
		t.Fatal(err)
	}

	h, err = NewJSONHashStore(f.Name(), HashStrategyRead)
	if err != nil {
		t.Fatal(err)
	}

	hash, err := h.Get("foo")
	if err != nil {
		t.Fatal(err)
	}

	if hash != "bar" {
		t.Fatal("Expected hash to match")
	}
}

func TestNewJSONHashStore_InvalidJSON(t *testing.T) {
	f, err := os.CreateTemp("", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewJSONHashStore(f.Name(), HashStrategyReadWrite)
	if err != nil {
		t.Fatal(err)
	}
}
