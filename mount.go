// Copyright 2015 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fuse

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// Server is an interface for any type that knows how to serve ops read from a
// connection.
type Server interface {
	// Read and serve ops from the supplied connection until EOF. Do not return
	// until all operations have been responded to. Must not be called more than
	// once.
	ServeOps(*Connection)

	// Stop readop
	Stop()
	// Check whether server is stopped by restart signal
	Stopped() bool
}

// Mount attempts to mount a file system on the given directory, using the
// supplied Server to serve connection requests. It blocks until the file
// system is successfully mounted.
func Mount(
	dir string,
	server Server,
	config *MountConfig) (mfs *MountedFileSystem, err error) {
	// Sanity check: make sure the mount point exists and is a directory. This
	// saves us from some confusing errors later on OS X.
	fi, err := os.Stat(dir)
	switch {
	case os.IsNotExist(err):
		return

	case err != nil:
		err = fmt.Errorf("Statting mount point: %v", err)
		return

	case !fi.IsDir():
		err = fmt.Errorf("Mount point %s is not a directory", dir)
		return
	}

	// Initialize the struct.
	mfs = &MountedFileSystem{
		dir:                 dir,
		joinStatusAvailable: make(chan struct{}),
	}

	// Begin the mounting process, which will continue in the background.
	ready := make(chan error, 1)
	dev, err := mount(dir, config, ready)
	if err != nil {
		err = fmt.Errorf("mount: %v", err)
		return
	}

	// Choose a parent context for ops.
	cfgCopy := *config
	if cfgCopy.OpContext == nil {
		cfgCopy.OpContext = context.Background()
	}

	// Create a Connection object wrapping the device.
	connection, err := newConnection(
		cfgCopy,
		config.DebugLogger,
		config.ErrorLogger,
		dev)

	if err != nil {
		err = fmt.Errorf("newConnection: %v", err)
		return
	}

	// Serve the connection in the background. When done, set the join status.
	go func() {
		server.ServeOps(connection)
		mfs.joinStatus = connection.close()
		close(mfs.joinStatusAvailable)
	}()

	// Wait for the mount process to complete.
	if err = <-ready; err != nil {
		err = fmt.Errorf("mount (background): %v", err)
		return
	}

	return
}

func triggerOps(dir string) {
	child := exec.Command("stat", dir)
	child.Start()
}

func MountWithGraceful(
	dir string,
	rundir string,
	server Server,
	config *MountConfig) (mfs *MountedFileSystem, err error) {
	// Sanity check: make sure the mount point exists and is a directory. This
	// saves us from some confusing errors later on OS X.
	fi, err := os.Stat(dir)
	switch {
	case os.IsNotExist(err):
		return

	case err != nil:
		err = fmt.Errorf("Statting mount point: %v", err)
		return

	case !fi.IsDir():
		err = fmt.Errorf("Mount point %s is not a directory", dir)
		return
	}

	// Initialize the struct.
	mfs = &MountedFileSystem{
		dir:                 dir,
		joinStatusAvailable: make(chan struct{}),
	}

	// Begin the mounting process, which will continue in the background.
	ready := make(chan error, 1)
	gr := NewGraceful(config.DebugLogger, rundir, "qfs-fuse")
	// Get file handle of /dev/fuse
	err = gr.GetDev(dir, config, ready)
	if err != nil {
		err = fmt.Errorf("mount: %v", err)
		return
	}

	// Choose a parent context for ops.
	cfgCopy := *config
	if cfgCopy.OpContext == nil {
		cfgCopy.OpContext = context.Background()
	}
	// Create connection
	err = gr.GetConnection(cfgCopy, config.DebugLogger, config.ErrorLogger, gr.dev)

	if err != nil {
		err = fmt.Errorf("newConnection: %v", err)
		return
	}
	// Serve the connection in the background. When done, set the join status.
	go func() {
		// If filesystem server is started by restart signal, child process
		// should wait SIGUSR1 from parent process, then read ops from /dev/fuse.
		gr.ShouldWaitParent(syscall.SIGUSR1)
		server.ServeOps(gr.conn)
		if st := server.Stopped(); st {
			// If filesystem server is stopped by restart signal,
			// parent process should wait until child process is started,
			// the channel will be closed in restart callback.
			// then main process exits.
			<-gr.stop
		}
		mfs.joinStatus = gr.conn.close()
		close(mfs.joinStatusAvailable)
	}()

	// Register restart callback
	err = gr.Ready(syscall.SIGHUP, func() error {
		child, err := gr.Restart()
		if err != nil {
			config.DebugLogger.Print("gracefule retart error: %v", err)
			if err == ErrGRestartChildNotReady {
				// Child is started but wait too long to be ready ready, we will still exit
			} else if err == ErrGRestartFailed {
				return err
				// Child is gone, we return to service
			}
		}
		// Stop readop of parent
		server.Stop()
		triggerOps(dir) // In case of parent is waiting for ops in kernel.
		if err := syscall.Kill(child, syscall.SIGUSR1); err != nil {
			config.DebugLogger.Println("Failed to end SIGUSR1 to child", child)
		}
		close(gr.stop)
		return err
	})

	if err != nil {
		err = fmt.Errorf("Not get ready.")
		return
	}

	// Wait for the mount process to complete.
	if err = <-ready; err != nil {
		err = fmt.Errorf("mount (background): %v", err)
		return
	}

	return
}
