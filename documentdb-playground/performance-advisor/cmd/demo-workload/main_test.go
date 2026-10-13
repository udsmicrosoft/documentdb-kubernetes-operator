// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkloadRequiresLocalAddressAndPrivateSyntheticCredentials(t *testing.T) {
	password := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(password, []byte(strings.Repeat("a", 48)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{
		"127.0.0.1:12345", "172.19.0.2:10260", "0.0.0.0:12345", "8.8.8.8:12345",
		"remote:12345", "127.0.0.1:http", "127.0.0.1:0", "127.0.0.1:65536",
	} {
		_, err := localOptions(address, password)
		if (err == nil) != (address == "127.0.0.1:12345" || address == "172.19.0.2:10260") {
			t.Fatalf("unexpected address admission: %s %v", address, err)
		}
	}
	if err := os.Chmod(password, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := localOptions("127.0.0.1:12345", password); err == nil {
		t.Fatal("publicly readable credentials accepted")
	}
	if err := os.Chmod(password, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "password-link")
	if err := os.Symlink(password, link); err != nil {
		t.Fatal(err)
	}
	if _, err := localOptions("127.0.0.1:12345", link); err == nil {
		t.Fatal("symlinked credentials accepted")
	}
	if err := os.WriteFile(password, []byte("not-a-generated-password"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := localOptions("127.0.0.1:12345", password); err == nil {
		t.Fatal("malformed credentials accepted")
	}
}
