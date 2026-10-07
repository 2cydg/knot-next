package sshserver

import (
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestServerBasic(t *testing.T) {
	srv := New(t, Config{
		User:     "testuser",
		Password: "testpass",
	})

	if srv.Addr() == "" {
		t.Fatal("server address is empty")
	}

	if srv.Port() == 0 {
		t.Fatal("server port is 0")
	}

	// Test connection
	config := &ssh.ClientConfig{
		User: "testuser",
		Auth: []ssh.AuthMethod{
			ssh.Password("testpass"),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         2 * time.Second,
	}

	client, err := ssh.Dial("tcp", srv.Addr(), config)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	defer session.Close()

	// Just test that we can create a session - don't run commands yet
	t.Logf("Successfully connected to test SSH server at %s", srv.Addr())
}
