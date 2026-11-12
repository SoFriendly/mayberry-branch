package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	if version := os.Getenv("MAYBERRY_TEST_UPDATE_VERSION"); version != "" && len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("mayberry " + version)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestUpdateExecutableMustRunAndMatchVersion(t *testing.T) {
	t.Setenv("MAYBERRY_TEST_UPDATE_VERSION", "fixture")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := checkUpdateExecutable(exe, "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := checkUpdateExecutable(exe, "other"); err == nil {
		t.Fatal("accepted mismatched version")
	}
	p := filepath.Join(t.TempDir(), "invalid.update")
	os.WriteFile(p, []byte("not an executable"), 0700)
	if err := checkUpdateExecutable(p, "fixture"); err == nil {
		t.Fatal("accepted unusable executable")
	}
}

func TestFailedWindowsReplacementRestoresInstalledFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mayberry.exe")
	if err := os.WriteFile(p, []byte("working executable"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := replaceExecutable(p, p+".missing", true); err == nil {
		t.Fatal("expected rename failure")
	}
	data, err := os.ReadFile(p)
	if err != nil || string(data) != "working executable" {
		t.Fatal("installed file was not restored", err)
	}
}
