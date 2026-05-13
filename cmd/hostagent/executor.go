// Package hostagent implements the command executor for the Landscape host agent.
// It receives commands from the Landscape server via a bidirectional gRPC stream
// and executes them on the local system.
package hostagent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	landscapeapi "github.com/canonical/landscape-hostagent-api"
	log "github.com/canonical/ubuntu-pro-for-wsl/common/grpc/logstreamer"
	"github.com/ubuntu/decorate"
	"github.com/ubuntu/gowsl"
	"google.golang.org/grpc"
)

// executor is in charge of executing commands received from the Landscape server.
type executor struct {
	serviceData
	sendStatus func(ctx context.Context, in *landscapeapi.CommandStatus, opts ...grpc.CallOption) (*landscapeapi.Empty, error)
}

// ExecInstall handles the Install command from Landscape synchronously.
// It downloads, imports, and configures the WSL instance, blocking until all
// steps are complete, and returns the final status to the caller directly.
func (e executor) ExecInstall(ctx context.Context, command *landscapeapi.Command) (*landscapeapi.CommandStatus, error) {
	cmd := command.GetInstall()
	if cmd == nil {
		return nil, errors.New("not an install command")
	}

	log.Infof(ctx, "Landscape: executing Install for %s", cmd.GetId())

	err := e.install(ctx, cmd)

	status := &landscapeapi.CommandStatus{
		CommandState: landscapeapi.CommandState_Completed,
		RequestId:    command.GetRequestId(),
	}
	if err != nil {
		status.Error = err.Error()
	}

	return status, nil
}

func (e executor) install(ctx context.Context, cmd *landscapeapi.Command_Install) (err error) {
	if cmd.GetId() == "" {
		return errors.New("empty distro name")
	}

	distro := gowsl.NewDistro(ctx, cmd.GetId())
	if registered, err := distro.IsRegistered(); err != nil {
		return err
	} else if registered {
		return errors.New("already installed")
	}

	defer func() {
		if err == nil {
			return
		}
		// Clean up on error.
		cleanupErr := distro.Uninstall(ctx)
		if cleanupErr != nil {
			log.Warningf(ctx, "Landscape Install: failed to clean up %q after failed Install: %v", distro.Name(), cleanupErr)
		}
	}()

	if rootfs := cmd.GetRootfsURL(); rootfs != "" {
		u, err := url.Parse(rootfs)
		if err != nil {
			return err
		}

		id := distro.Name()
		reserved := regexp.MustCompile(`^(?i)Ubuntu-[0-9]{2}\.[0-9]{2}$`)
		if strings.EqualFold(id, "Ubuntu") || strings.EqualFold(id, "Ubuntu-Preview") || reserved.Match([]byte(id)) {
			return fmt.Errorf("target distro ID %s is reserved and should not be used for custom instances", id)
		}

		if err = installFromURL(ctx, e.homeDir(), e.downloadDir(), distro, u); err != nil {
			return err
		}
	} else {
		if err = installFromWSLOnlineDistros(ctx, distro); err != nil {
			return err
		}
	}

	// Wait for the instance to be ready.
	sleep := distro.Command(ctx, "sleep 5")
	if err := sleep.Run(); err != nil {
		return fmt.Errorf("could not wait for distro to start: %v", err)
	}

	return nil
}

// installFromWSLOnlineDistros installs a distro by means of `wsl --install`.
func installFromWSLOnlineDistros(ctx context.Context, distro gowsl.Distro) (err error) {
	defer decorate.OnError(&err, "can't install from Microsoft Store")

	if err := gowsl.Install(ctx, distro.Name()); err != nil {
		return err
	}

	return nil
}

// installFromURL downloads a rootfs tarball and imports it as a new WSL distro.
// This operation can take several minutes for large images (500MB-2GB).
func installFromURL(ctx context.Context, homeDir string, downloadDir string, distro gowsl.Distro, rootfsURL *url.URL) (err error) {
	defer decorate.OnError(&err, "can't install from URL: %q", rootfsURL)

	tmpDir := filepath.Join(downloadDir, distro.Name())
	if err := os.MkdirAll(tmpDir, 0700); err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	tarball := filepath.Join(tmpDir, distro.Name()+".tar.gz")

	err = download(ctx, rootfsURL, tarball)
	if err != nil {
		return err
	}

	// Create the directory that will contain the vhdx.
	vhdxDir := filepath.Join(homeDir, "WSL", distro.Name())
	if err := os.MkdirAll(vhdxDir, 0700); err != nil {
		return err
	}

	if _, err := gowsl.Import(ctx, distro.Name(), tarball, vhdxDir); err != nil {
		rmErr := os.RemoveAll(vhdxDir)
		if rmErr != nil {
			log.Warningf(ctx, "could not cleanup install directory: %v", rmErr)
		}
		return err
	}

	log.Debugf(ctx, "Distro %s installed successfully", distro.Name())
	return nil
}

// download downloads the rootfs from the given URL and writes it to the given
// destination while verifying its checksum.
func download(ctx context.Context, u *url.URL, destination string) (err error) {
	defer decorate.OnError(&err, "could not download %q", u)

	checksum, err := wantRootfsChecksum(ctx, u)
	if err != nil {
		return err
	}

	resp, err := http.Get(u.String())
	if err != nil {
		return err
	}

	if resp.StatusCode >= 400 {
		return fmt.Errorf("http request failed with code %d", resp.StatusCode)
	}

	f, err := os.Create(destination)
	if err != nil {
		return err
	}
	defer f.Close()

	r := io.TeeReader(resp.Body, f)
	if checksum != "" {
		match, err := checksumMatches(ctx, r, checksum)
		if err != nil {
			return err
		}
		if !match {
			return fmt.Errorf("checksum %s for %s does not match", checksum, u)
		}
	} else {
		if _, err := io.Copy(io.Discard, r); err != nil {
			return err
		}
	}

	return nil
}

// wantRootfsChecksum fetches the checksum from the SHA256SUMS file found
// alongside the rootfs URL matching the rootfs file name.
func wantRootfsChecksum(ctx context.Context, u *url.URL) (string, error) {
	imageName := filepath.Base(u.Path)
	shasRelativeURL, err := url.Parse("SHA256SUMS")
	if err != nil {
		return "", fmt.Errorf("could not assemble SHA256SUMS location: %v", err)
	}
	checksumsURL := u.ResolveReference(shasRelativeURL)

	resp, err := http.Get(checksumsURL.String())
	if err != nil {
		return "", fmt.Errorf("could not download checksums file %q: %v", checksumsURL, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		log.Infof(ctx, "checksums file %q not found", checksumsURL)
		return "", nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read checksums file: %v", err)
	}

	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == imageName && len(fields[0]) > 0 {
			return fields[0], nil
		}
	}

	return "", fmt.Errorf("could not find checksum for %s in %s", imageName, checksumsURL)
}

func checksumMatches(ctx context.Context, reader io.Reader, wantChecksum string) (match bool, err error) {
	defer decorate.OnError(&err, "error checking checksum for: %q", reader)

	h := sha256.New()
	if _, err := io.Copy(h, reader); err != nil {
		return false, err
	}
	gotChecksum := fmt.Sprintf("%x", h.Sum(nil))
	log.Debugf(ctx, "Want checksum: %s, Got checksum: %s", wantChecksum, gotChecksum)

	return wantChecksum == gotChecksum, nil
}
