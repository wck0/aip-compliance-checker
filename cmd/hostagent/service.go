// Package hostagent implements the LandscapeHostAgent gRPC service.
package hostagent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"time"

	pb "github.com/canonical/landscape-hostagent-api"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Service implements the LandscapeHostAgent gRPC service.
type Service struct {
	pb.UnimplementedLandscapeHostAgentServer
	instanceDir string
}

// NewService creates a new hostagent service.
func NewService(instanceDir string) *Service {
	return &Service{instanceDir: instanceDir}
}

// Install handles the installation of a new WSL instance. It downloads the rootfs
// image (if provided), imports it, and registers the instance with Landscape.
// This operation may take several minutes depending on network speed and image size.
func (s *Service) Install(ctx context.Context, req *pb.Command) (*pb.CommandStatus, error) {
	install := req.GetInstall()
	if install == nil {
		return nil, status.Error(codes.InvalidArgument, "missing install command")
	}

	if install.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "instance id is required")
	}

	result := &pb.CommandStatus{
		RequestId:    req.RequestId,
		CommandState: pb.CommandState_InProgress,
	}

	// Download the rootfs image - this is a long-running operation that can take
	// several minutes for large images (typically 500MB-2GB).
	rootfsPath := fmt.Sprintf("%s/%s.tar.gz", s.instanceDir, install.Id)
	if install.RootfsURL != nil {
		if err := downloadRootfs(*install.RootfsURL, rootfsPath); err != nil {
			result.CommandState = pb.CommandState_Completed
			result.Error = fmt.Sprintf("failed to download rootfs: %v", err)
			return result, nil
		}
	}

	// Import the instance - another long-running step that involves extracting
	// and configuring the filesystem.
	if err := importInstance(install.Id, rootfsPath); err != nil {
		result.CommandState = pb.CommandState_Completed
		result.Error = fmt.Sprintf("failed to import instance: %v", err)
		return result, nil
	}

	// Apply cloud-init configuration if provided.
	if install.Cloudinit != nil {
		if err := applyCloudInit(install.Id, *install.Cloudinit); err != nil {
			result.CommandState = pb.CommandState_Completed
			result.Error = fmt.Sprintf("failed to apply cloud-init: %v", err)
			return result, nil
		}
	}

	// Start the instance after installation.
	if err := startInstance(install.Id); err != nil {
		result.CommandState = pb.CommandState_Completed
		result.Error = fmt.Sprintf("failed to start instance: %v", err)
		return result, nil
	}

	result.CommandState = pb.CommandState_Completed
	return result, nil
}

// downloadRootfs downloads a rootfs image from the given URL to the destination path.
// For large images this may take several minutes.
func downloadRootfs(url, destPath string) error {
	client := &http.Client{
		Timeout: 30 * time.Minute,
	}

	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("download: %w", err)
	}

	return nil
}

// importInstance imports a rootfs tarball as a new WSL instance.
func importInstance(id, rootfsPath string) error {
	cmd := exec.Command("wsl", "--import", id, fmt.Sprintf("/var/lib/wsl/%s", id), rootfsPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("wsl import failed: %s: %w", string(output), err)
	}
	return nil
}

// applyCloudInit writes and applies a cloud-init configuration to the instance.
func applyCloudInit(id, config string) error {
	configPath := fmt.Sprintf("/var/lib/wsl/%s/cloud-init.yaml", id)
	if err := os.WriteFile(configPath, []byte(config), 0644); err != nil {
		return fmt.Errorf("write cloud-init config: %w", err)
	}

	cmd := exec.Command("wsl", "-d", id, "cloud-init", "single", "--name", "cc_write_files")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cloud-init apply failed: %s: %w", string(output), err)
	}
	return nil
}

// startInstance starts a WSL instance.
func startInstance(id string) error {
	cmd := exec.Command("wsl", "-d", id, "echo", "started")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("start failed: %s: %w", string(output), err)
	}
	return nil
}
