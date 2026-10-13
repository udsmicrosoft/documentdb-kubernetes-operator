// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/documentdb/documentdb-kubernetes-operator/documentdb-playground/performance-advisor/demo"
	"github.com/documentdb/documentdb-kubernetes-operator/documentdb-playground/performance-advisor/workload"
	driver "go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func localOptions(address, passwordPath string) (*options.ClientOptions, error) {
	host, port, err := net.SplitHostPort(address)
	ip := net.ParseIP(host)
	if err != nil || ip.To4() == nil || (!ip.IsLoopback() && !ip.IsPrivate()) ||
		!regexp.MustCompile(`^[1-9][0-9]{0,4}$`).MatchString(port) {
		return nil, fmt.Errorf("workload requires an IPv4 loopback or private bridge address and numeric port")
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return nil, fmt.Errorf("workload port is outside the TCP port range")
	}
	info, err := os.Lstat(passwordPath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() != 49 {
		return nil, fmt.Errorf("expected a private synthetic password file")
	}
	data, err := os.ReadFile(passwordPath)
	if err != nil {
		return nil, err
	}
	password := strings.TrimSpace(string(data))
	if !regexp.MustCompile(`^[a-f0-9]{48}$`).MatchString(password) {
		return nil, fmt.Errorf("invalid synthetic password")
	}
	return options.Client().SetHosts([]string{address}).SetDirect(true).
		SetAuth(options.Credential{Username: "telemetry_user", Password: password, AuthSource: "admin"}).
		SetTLSConfig(&tls.Config{
			MinVersion: tls.VersionTLS12,
			// This isolated local demo uses the image's generated certificate.
			InsecureSkipVerify: true,
		}).
		SetServerSelectionTimeout(3 * time.Second).SetConnectTimeout(3 * time.Second).
		SetTimeout(10 * time.Second).SetMaxPoolSize(1), nil
}

func waitReady(ctx context.Context, client *driver.Client) error {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	for {
		err := client.Ping(ctx, nil)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("local database readiness: %w", errors.Join(context.Cause(ctx), err))
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func run(ctx context.Context, action, address, passwordPath, dir string) (err error) {
	if action != "ready" && action != "run" {
		return fmt.Errorf("action must be ready or run")
	}
	opts, err := localOptions(address, passwordPath)
	if err != nil {
		return err
	}
	client, err := driver.Connect(opts)
	if err != nil {
		return fmt.Errorf("connect local workload: %w", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err = errors.Join(err, client.Disconnect(cleanup))
	}()
	if err := waitReady(ctx, client); err != nil {
		return err
	}
	if action == "ready" {
		fmt.Println("docdb-local authenticated wire-protocol readiness passed")
		return nil
	}
	state, err := demo.LoadRun(dir)
	if err != nil {
		return err
	}
	gate, err := demo.OpenCoordinator(ctx, dir, state.CaptureID)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, gate.Close()) }()
	database := client.Database("live_telemetry_" + state.ID)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		err = errors.Join(err, database.Drop(cleanup))
	}()
	return workload.Run(ctx, database.Collection("observations"), gate, func(trial demo.Trial) error {
		fmt.Printf("live-telemetry-trial id=%s start=%s end=%s operations=%d\n",
			trial.ID, trial.Start.Format(time.RFC3339Nano), trial.End.Format(time.RFC3339Nano), trial.Operations)
		return nil
	})
}

func main() {
	action := flag.String("action", "run", "ready or run")
	address := flag.String("address", "", "Owned local wire-protocol listener")
	password := flag.String("password-file", "", "Private synthetic password file")
	dir := flag.String("dir", "", "Prepared private run directory")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional arguments")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, timeout := context.WithTimeout(ctx, demo.RunTimeout)
	defer timeout()
	if err := run(ctx, *action, *address, *password, *dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
