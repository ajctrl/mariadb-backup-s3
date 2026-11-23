package backup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The import runs on the session that owns GET_LOCK. Backup workers inherit
// its stdin so an orphaned dump cannot release the lock by losing its parent.
func (a *App) withServerLock(ctx context.Context, r commandRunner, restore bool, run func(context.Context, commandRunner) error) error {
	session, stop := context.WithCancel(context.WithoutCancel(ctx))
	defer stop()
	args := append(a.connectionArgs(), "--batch", "--unbuffered", "--skip-reconnect", "--skip-column-names", "--skip-print-query-on-error")
	if restore {
		args = append(args, "--binary-mode", "--max-allowed-packet=1G")
	}
	cmd, closeFiles, err := r.command(session, "mariadb", args...)
	if err != nil {
		return err
	}
	defer closeFiles()
	var diagnostic restoreDiagnostic
	if restore && !a.Config.RestoreLogErrorDetails {
		cmd.Stderr = &diagnostic
	}
	input, feed, err := os.Pipe()
	if err != nil {
		return err
	}
	defer input.Close()
	defer feed.Close()
	reply, output, err := os.Pipe()
	if err != nil {
		return err
	}
	defer reply.Close()
	defer output.Close()
	cmd.Stdin, cmd.Stdout = input, output
	if err = cmd.Start(); err != nil {
		return err
	}
	input.Close()
	output.Close()
	exited := make(chan struct{})
	var clientErr error
	go func() { clientErr = cmd.Wait(); close(exited) }()
	defer func() { stop(); <-exited }()
	reader := bufio.NewReader(reply)
	client := lockClient{feed: feed, reply: reply, reader: reader}

	stopAcquire := context.AfterFunc(ctx, stop)
	// A short global idle timeout must not release the lock between heartbeats.
	line, err := client.query("SET SESSION wait_timeout=GREATEST(@@wait_timeout,300); SELECT GET_LOCK('mariadb-backup-s3',0), @@lower_case_table_names, @@max_allowed_packet, @@sql_mode")
	stopAcquire()
	if err != nil {
		stop()
		<-exited
		return fmt.Errorf("could not acquire MariaDB backup lock: %w", errors.Join(err, diagnostic.err(0)))
	}
	fields := strings.Split(line, "\t")
	if fields[0] != "1" {
		return errors.New("server is busy or the lock has been lost")
	}
	if len(fields) != 4 {
		return errors.New("invalid MariaDB lock response")
	}
	switch fields[1] {
	case "0":
	case "1", "2":
		r.foldDatabaseNames = true
	default:
		return errors.New("invalid lower_case_table_names")
	}
	packet, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || packet < 2 {
		return errors.New("invalid MariaDB max_allowed_packet")
	}
	job, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stopMonitor := client.monitor(job, cancel, exited)
	defer stopMonitor()
	if restore {
		r.importSQL = func(file *os.File) error {
			if err := checkPacketSize(job, file, min(packet, 1<<30), fields[3]); err != nil {
				return err
			}
			var prefix string
			if a.Config.BackupMode != "combined" {
				existing, err := destinationDefaults(client.query, a.Config.Database)
				if err != nil {
					return err
				}
				prefix, err = a.prepareDatabase(job, file, existing, r.foldDatabaseNames, min(packet, 1<<30), fields[3], client.query)
				if err != nil {
					return err
				}
				if existing != nil {
					current, err := destinationDefaults(client.query, a.Config.Database)
					if err != nil {
						return err
					}
					if current == nil || *current != *existing {
						return errors.New("destination database changed during preflight; no SQL imported")
					}
				}
			}
			stopMonitor()
			stopImport := context.AfterFunc(job, stop)
			defer stopImport()
			if err := client.check(); err != nil {
				return fmt.Errorf("restore lock: %w", err)
			}
			if err := reply.SetReadDeadline(time.Time{}); err != nil {
				return err
			}
			// Consume results concurrently so a stored query cannot fill stdout
			// and block the client while Go is still writing the dump.
			drained := make(chan error, 1)
			go func() { _, err := io.Copy(io.Discard, reader); drained <- err }()
			_, writeErr := io.Copy(feed, io.MultiReader(strings.NewReader(prefix), file))
			feed.Close()
			<-exited
			return errors.Join(writeErr, <-drained, commandError(job, "mariadb", clientErr), diagnostic.err(client.preambleLines+strings.Count(prefix, "\n")))
		}
	} else {
		r.lease = feed
	}
	err = run(job, r)
	stopMonitor()
	if !restore && err == nil && job.Err() == nil {
		err = client.check()
	}
	return errors.Join(err, context.Cause(job))
}

type lockClient struct {
	mu            sync.Mutex // serialize metadata queries and heartbeat replies
	feed, reply   *os.File
	reader        *bufio.Reader
	preambleLines int
}

func (c *lockClient) query(sql string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := io.WriteString(c.feed, sql+";\n"); err != nil {
		return "", err
	}
	if err := c.reply.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return "", err
	}
	c.preambleLines++
	line, err := c.reader.ReadString('\n')
	return strings.TrimSuffix(line, "\n"), err
}

func (c *lockClient) check() error {
	line, err := c.query("SELECT IS_USED_LOCK('mariadb-backup-s3') = CONNECTION_ID()")
	if err == nil && line != "1" {
		err = errors.New("server is busy or the lock has been lost")
	}
	return err
}

func (c *lockClient) monitor(ctx context.Context, cancel context.CancelCauseFunc, exited <-chan struct{}) func() {
	finished, monitorDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(monitorDone)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-finished:
				return
			case <-ctx.Done():
				return
			case <-exited:
				cancel(errors.New("MariaDB backup lock connection closed"))
				return
			case <-ticker.C:
				if err := c.check(); err != nil {
					cancel(fmt.Errorf("MariaDB backup lock lost: %w", err))
					return
				}
			}
		}
	}()
	return sync.OnceFunc(func() { close(finished); <-monitorDone })
}
