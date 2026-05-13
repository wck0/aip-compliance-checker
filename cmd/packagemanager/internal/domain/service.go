// Package domain implements the core business logic for the package manager service.
// It defines the ports (interfaces) and the service implementation following
// hexagonal architecture principles as used in landscape-go.
package domain

import (
	"context"
	"fmt"
	"time"
)

// PackageChange represents a single package operation to apply.
type PackageChange struct {
	Name    string
	Version string
	Action  string // "install", "remove", "upgrade"
}

// PackageResult represents the outcome of a single package operation.
type PackageResult struct {
	Name    string
	Version string
	Action  string
	Status  string // "success", "failed"
	Error   string
}

// ApplyResult represents the outcome of an ApplyPackages call.
type ApplyResult struct {
	MachineID string
	Results   []PackageResult
	StartedAt time.Time
	EndedAt   time.Time
}

// PackageManagerPort is the inbound port interface for the package manager service.
type PackageManagerPort interface {
	// ApplyPackages applies a set of package changes to the specified machines.
	// This operation contacts each machine, transfers package data, runs apt/dpkg,
	// and waits for completion. For large deployments this may take many minutes.
	ApplyPackages(ctx context.Context, machineIDs []string, changes []PackageChange) ([]ApplyResult, error)
}

// MachineConnector is the adapter interface for connecting to managed machines.
type MachineConnector interface {
	// ExecutePackageChanges connects to a machine and applies the given changes.
	// This involves transferring package data over the network and running the
	// package manager on the remote machine.
	ExecutePackageChanges(ctx context.Context, machineID string, changes []PackageChange) ([]PackageResult, error)
}

// PackageManagerService implements PackageManagerPort.
type PackageManagerService struct {
	connector MachineConnector
}

// NewPackageManagerService creates a new PackageManagerService.
func NewPackageManagerService(connector MachineConnector) *PackageManagerService {
	return &PackageManagerService{
		connector: connector,
	}
}

// ApplyPackages applies package changes to a set of machines synchronously.
// It iterates through each machine sequentially, applying all package changes
// and collecting results. The caller is blocked until all machines have been
// processed, which may take several minutes for large deployments.
//
// AIP-151 requires that methods which may take a significant amount of time to
// complete should return a google.longrunning.Operation, allowing the client to
// poll for completion. This method violates that requirement by blocking until
// all work is done and returning the final result directly.
func (s *PackageManagerService) ApplyPackages(ctx context.Context, machineIDs []string, changes []PackageChange) ([]ApplyResult, error) {
	if len(machineIDs) == 0 {
		return nil, fmt.Errorf("at least one machine ID is required")
	}
	if len(changes) == 0 {
		return nil, fmt.Errorf("at least one package change is required")
	}

	var results []ApplyResult

	for _, machineID := range machineIDs {
		startedAt := time.Now()

		packageResults, err := s.connector.ExecutePackageChanges(ctx, machineID, changes)
		if err != nil {
			return nil, fmt.Errorf("failed to apply changes to machine %s: %w", machineID, err)
		}

		results = append(results, ApplyResult{
			MachineID: machineID,
			Results:   packageResults,
			StartedAt: startedAt,
			EndedAt:   time.Now(),
		})
	}

	return results, nil
}
