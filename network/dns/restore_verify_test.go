package dns

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

type mockPodmanClient struct {
	mu             sync.Mutex
	connectCalls   []connectCall
	resolvBody     string
	connectErr     error
	execErr        error
	execStderr     string
}

type connectCall struct {
	container string
	network   string
}

func (m *mockPodmanClient) ConnectNetwork(_ context.Context, container, network string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connectCalls = append(m.connectCalls, connectCall{container, network})
	return m.connectErr
}

func (m *mockPodmanClient) ExecInContainer(_ context.Context, _ string, _ []string) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resolvBody, m.execStderr, m.execErr
}

func TestVerifyAfterRestoreSuccess(t *testing.T) {
	t.Parallel()
	mock := &mockPodmanClient{
		resolvBody: "# generated\nnameserver " + LinkLocalDNSAddr + "\noptions ndots:0 timeout:1 attempts:2\n",
	}
	if err := VerifyAfterRestore(context.Background(), "cap-1", "falak-billing", mock); err != nil {
		t.Fatalf("VerifyAfterRestore: %v", err)
	}
	if len(mock.connectCalls) != 1 {
		t.Fatalf("ConnectNetwork called %d times, want 1", len(mock.connectCalls))
	}
	if mock.connectCalls[0].container != "cap-1" || mock.connectCalls[0].network != "falak-billing" {
		t.Fatalf("ConnectNetwork wrong args: %+v", mock.connectCalls[0])
	}
}

func TestVerifyAfterRestoreMismatch(t *testing.T) {
	t.Parallel()
	mock := &mockPodmanClient{
		resolvBody: "nameserver 1.1.1.1\n",
	}
	err := VerifyAfterRestore(context.Background(), "cap-2", "falak-billing", mock)
	if err == nil {
		t.Fatal("expected mismatch error, got nil")
	}
	if !errors.Is(err, ErrRestoreVerifyMismatch) {
		t.Fatalf("error not wrapped: %v", err)
	}
}

func TestVerifyAfterRestoreConnectError(t *testing.T) {
	t.Parallel()
	want := fmt.Errorf("network gone")
	mock := &mockPodmanClient{connectErr: want}
	err := VerifyAfterRestore(context.Background(), "cap-3", "falak-billing", mock)
	if err == nil || !errors.Is(err, want) {
		t.Fatalf("expected wrapped connect error, got %v", err)
	}
}

func TestVerifyAfterRestoreExecError(t *testing.T) {
	t.Parallel()
	want := fmt.Errorf("exec failed")
	mock := &mockPodmanClient{execErr: want, execStderr: "boom"}
	err := VerifyAfterRestore(context.Background(), "cap-4", "falak-billing", mock)
	if err == nil || !errors.Is(err, want) {
		t.Fatalf("expected wrapped exec error, got %v", err)
	}
}

func TestVerifyAfterRestoreValidation(t *testing.T) {
	t.Parallel()
	mock := &mockPodmanClient{resolvBody: "nameserver " + LinkLocalDNSAddr}
	if err := VerifyAfterRestore(context.Background(), "", "net", mock); err == nil {
		t.Fatal("expected error for empty container")
	}
	if err := VerifyAfterRestore(context.Background(), "c", "", mock); err == nil {
		t.Fatal("expected error for empty network")
	}
	if err := VerifyAfterRestore(context.Background(), "c", "n", nil); err == nil {
		t.Fatal("expected error for nil client")
	}
}
