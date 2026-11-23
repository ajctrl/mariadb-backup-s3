package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ajctrl/mariadb-backup-s3/internal/tempfile"
)

type commandRunner struct {
	defaults          string
	passphrase        string
	stderr            io.Writer
	lock              *os.File
	lease             *os.File
	foldDatabaseNames bool
	importSQL         func(*os.File) error
}

func (r commandRunner) run(ctx context.Context, stdout io.Writer, name string, args ...string) error {
	return r.runInput(ctx, nil, stdout, name, args...)
}

func (r commandRunner) runInput(ctx context.Context, stdin io.Reader, stdout io.Writer, name string, args ...string) error {
	cmd, closeFiles, err := r.command(ctx, name, args...)
	if err != nil {
		return err
	}
	defer closeFiles()
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	return commandError(ctx, name, cmd.Run())
}

func (r commandRunner) command(ctx context.Context, name string, args ...string) (*exec.Cmd, func(), error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = r.stderr
	cmd.Env = withoutEnv(withoutEnv(withoutEnv(os.Environ(), "MYSQL_PWD"), "MARIADB_PASSWORD"), "PASSPHRASE")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if r.lock != nil {
		cmd.ExtraFiles = []*os.File{r.lock}
	}
	closeFiles := func() {}
	if name == "mariadb" || name == "mariadb-dump" || name == "gpg" {
		fd := 3 + len(cmd.ExtraFiles)
		secret := r.defaults
		options := []string{fmt.Sprintf("--defaults-file=/proc/self/fd/%d", fd)}
		if name == "gpg" {
			secret = r.passphrase
			options = []string{"--passphrase-fd", fmt.Sprint(fd)}
		}
		f, err := anonymousCredential(secret)
		if err != nil {
			return nil, nil, err
		}
		cmd.ExtraFiles = append(cmd.ExtraFiles, f)
		cmd.Args = append(append([]string{cmd.Args[0]}, options...), args...)
		closeFiles = func() { f.Close() }
	}
	if r.lease != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, r.lease)
	}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd, closeFiles, nil
}

// Unlink before writing a secret. Each child receives a separate file offset.
func anonymousCredential(value string) (*os.File, error) {
	f, err := tempfile.Open("/tmp", "mariadb-credential-*")
	if err != nil {
		return nil, err
	}
	_, err = io.WriteString(f, value)
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func commandError(ctx context.Context, name string, err error) error {
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Keep command arguments and configuration out of error messages.
		return fmt.Errorf("%s failed: %w", name, err)
	}
	return nil
}

type pipelineCommand struct {
	name string
	args []string
}

// pipeline waits for every process, including a producer that closes stdout
// before reporting an error. Only its caller may signal EOF to the uploader.
func (r commandRunner) pipeline(ctx context.Context, stdout io.Writer, producer pipelineCommand, filter *pipelineCommand) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	dump, closeDumpFiles, err := r.command(ctx, producer.name, producer.args...)
	if err != nil {
		return err
	}
	defer closeDumpFiles()
	// Own the read end so we can observe EOF even if the dump process keeps running.
	source, dumpOut, err := os.Pipe()
	if err != nil {
		return err
	}
	defer source.Close()
	defer dumpOut.Close()
	dump.Stdout = dumpOut

	destination := stdout
	var filterInput io.WriteCloser
	done := make(chan error, 3)
	pending := 2 // dump process and plaintext copy
	if filter != nil {
		cmd, closeFiles, err := r.command(ctx, filter.name, filter.args...)
		if err != nil {
			return err
		}
		defer closeFiles()
		cmd.Stdout = stdout
		// A slow upload can still be draining after GPG exits.
		cmd.WaitDelay = 0
		filterInput, err = cmd.StdinPipe()
		if err != nil {
			return err
		}
		if err := cmd.Start(); err != nil {
			return commandError(ctx, filter.name, err)
		}
		destination = filterInput
		pending++
		go func() { done <- commandError(ctx, filter.name, cmd.Wait()) }()
	}

	err = dump.Start()
	dumpOut.Close() // Only the child may keep the write end open.
	if err != nil {
		done <- commandError(ctx, producer.name, err)
	} else {
		go func() { done <- commandError(ctx, producer.name, dump.Wait()) }()
	}
	go func() {
		n, err := io.Copy(destination, source)
		if filterInput != nil {
			filterInput.Close()
		}
		if err == nil && n == 0 {
			err = errors.New("no backup data to upload")
		}
		done <- err
	}()

	var result error
	for range pending {
		if err := <-done; err != nil {
			result = errors.Join(result, err)
			cancel()
			source.Close()
		}
	}
	return result
}

func withoutEnv(environ []string, name string) []string {
	result := make([]string, 0, len(environ))
	for _, entry := range environ {
		if !strings.HasPrefix(entry, name+"=") {
			result = append(result, entry)
		}
	}
	return result
}

func acquireLock(path string) (*os.File, error) {
	dir := filepath.Dir(path)
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("unsafe backup lock directory %q", dir)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, fmt.Errorf("open backup lock: %w", err)
	}
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		file.Close()
		return nil, fmt.Errorf("unsafe backup lock %q", path)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("Could not acquire backup lock; another backup may already be running: %w", err)
	}
	// Close releases the lock. Never unlink it or explicitly unlock the shared
	// file description: any surviving child must retain its inherited lock.
	return file, nil
}
