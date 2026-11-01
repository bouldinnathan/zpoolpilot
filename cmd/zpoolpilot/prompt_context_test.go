// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestConfirmationCancellationDoesNotWaitForInput(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	out, err := os.CreateTemp(t.TempDir(), "prompt")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	a := &app{reader: bufio.NewReader(reader), out: out}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := a.readConfirmation(ctx, "Approve [Y/n]: ")
		done <- err
	}()
	// Wait for the prompt to appear before simulating a signal.
	deadline := time.After(time.Second)
	for {
		info, err := out.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("confirmation did not display its prompt")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("confirmation error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled confirmation continued waiting for input")
	}
}

func TestConfirmationRejectsUndisplayedPromptAndIncompleteInput(t *testing.T) {
	out, err := os.CreateTemp(t.TempDir(), "prompt")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	for _, input := range []string{"", "yes"} {
		a := &app{out: out, reader: bufio.NewReader(strings.NewReader(input))}
		if _, err := a.readConfirmation(context.Background(), "Approve [Y/n]: "); !errors.Is(err, io.EOF) {
			t.Fatalf("incomplete input %q error=%v", input, err)
		}
	}
	input := bufio.NewReader(strings.NewReader("yes\n"))
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	a := &app{out: out, reader: input}
	if _, err := a.readConfirmation(context.Background(), "Approve [Y/n]: "); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed output error=%v", err)
	}
	if unread, err := input.ReadString('\n'); unread != "yes\n" || err != nil {
		t.Fatalf("input was consumed without displaying prompt: %q, %v", unread, err)
	}
}

func TestConfirmationRejectsAlreadyCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&app{}).readConfirmation(ctx, "Approve: "); !errors.Is(err, context.Canceled) {
		t.Fatalf("already canceled prompt error=%v", err)
	}
}
