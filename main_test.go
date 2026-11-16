package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"k8s.io/client-go/rest"
)

func TestKubeClientsLoadsConfigOnceOnFirstAsk(t *testing.T) {
	loads := 0
	kube := newKubeClients(func() (*rest.Config, error) {
		loads++
		return &rest.Config{Host: "https://127.0.0.1:1"}, nil
	})
	if loads != 0 {
		t.Fatalf("config loaded %d times before any feature asked", loads)
	}

	first, err := kube.Clientset()
	if err != nil {
		t.Fatalf("Clientset: %v", err)
	}
	second, err := kube.Clientset()
	if err != nil {
		t.Fatalf("Clientset: %v", err)
	}
	if first != second {
		t.Fatal("two asks got two clientsets")
	}
	d1, err := kube.Dynamic()
	if err != nil {
		t.Fatalf("Dynamic: %v", err)
	}
	d2, err := kube.Dynamic()
	if err != nil {
		t.Fatalf("Dynamic: %v", err)
	}
	if d1 != d2 {
		t.Fatal("two asks got two dynamic clients")
	}
	if loads != 1 {
		t.Fatalf("config loaded %d times, want 1", loads)
	}
}

func TestKubeClientsFailureFailsEveryAsk(t *testing.T) {
	loads := 0
	kube := newKubeClients(func() (*rest.Config, error) {
		loads++
		return nil, errors.New("no kubeconfig")
	})
	if _, err := kube.Clientset(); err == nil {
		t.Fatal("Clientset: want the load error")
	}
	if _, err := kube.Dynamic(); err == nil {
		t.Fatal("Dynamic: want the load error")
	}
	if loads != 1 {
		t.Fatalf("config loaded %d times, want 1", loads)
	}
}

func TestOrganizationNamesFallsBackWithoutClient(t *testing.T) {
	kube := newKubeClients(func() (*rest.Config, error) {
		return nil, errors.New("no kubeconfig")
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if got := organizationNames(context.Background(), kube, logger); got != nil {
		t.Fatalf("organizationNames = %v, want nil (namespace labels)", got)
	}
}
