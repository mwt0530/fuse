package fuse

/*

Refer to:

	http://blog.scalingo.com/post/105609534953/graceful-server-restart-with-go
	http://grisha.org/blog/2014/06/03/graceful-restart-in-golang/
	http://supervisord.org/subprocess.html
	https://github.com/Supervisor/supervisor/issues/53

*/

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"git.qingstor.dev/global/common/log"
)

var ErrGRestartFailed = errors.New("gracefule restart failed, child exited")
var ErrGRestartChildNotReady = errors.New("graceful restart triggered but child is not ready")

type GRestartCallback func() error

type Graceful struct {
	dev            *os.File // file of /dev/fuse
	conn           *Connection
	startNoti      chan os.Signal // start signal from parent process
	stop           chan struct{}  // wait on stop channel until child process is started
	recoveredVer   []int
	recovered      bool
	runDir         string
	progName       string
	restartNoti    chan os.Signal
	restarting     bool
	restartCb      GRestartCallback
	pidFile        string
	logger         log.Logger
	lock           sync.Mutex
	RestartTimeout int // In seconds, default 60s
}

func NewGraceful(logger log.Logger, runDir string, progName string) *Graceful {
	gr := new(Graceful)
	gr.startNoti = make(chan os.Signal, 1)
	gr.stop = make(chan struct{})
	gr.runDir = runDir
	gr.recoveredVer = make([]int, 0)
	gr.progName = progName
	gr.restartNoti = make(chan os.Signal, 1)
	gr.logger = logger
	gr.checkRecover()
	gr.RestartTimeout = 60
	return gr
}

func (gr *Graceful) GetDev(
	dir string,
	cfg *MountConfig,
	ready chan<- error) (err error) {
	if gr.recovered {
		// On linux, mounting is never delayed.
		ready <- nil
		var fd int = 3
		gr.dev = os.NewFile(uintptr(fd), "/dev/fuse")
	} else {
		gr.dev, err = mount(dir, cfg, ready)
	}
	return
}

func (gr *Graceful) GetConnection(
	cfg MountConfig,
	debugLogger log.Logger,
	errorLogger log.Logger,
	dev *os.File) (err error) {
	if gr.recovered {
		env := os.Getenv("_GRACEFUL_RESTART")
		if env == "" {
			err = fmt.Errorf("Cannot get fuse version")
			return
		}
		version := strings.Split(env, ",")
		if len(version) != 2 {
			err = fmt.Errorf("Invalid version length")
			return
		}

		gr.conn = &Connection{
			cfg:         cfg,
			debugLogger: debugLogger,
			errorLogger: errorLogger,
			dev:         dev,
			cancelFuncs: make(map[uint64]func()),
		}
		major, err := strconv.Atoi(version[0])
		if err != nil {
			return err
		}

		minor, err := strconv.Atoi(version[1])
		if err != nil {
			return err
		}
		gr.conn.protocol.Major = uint32(major)
		gr.conn.protocol.Minor = uint32(minor)

	} else {
		gr.conn, err = newConnection(cfg, debugLogger, errorLogger, dev)
	}
	return
}

// When program start, should check whether recover from graceful restart
func (gr *Graceful) checkRecover() {
	var err error
	var v int
	env := os.Getenv("_GRACEFUL_RESTART")
	if env == "" {
		return
	}
	gr.recovered = true
	gr.logger.Printf("Process %d recovering fd %v", os.Getpid(), env)
	version := strings.Split(env, ",")
	for _, val := range version {
		v, err = strconv.Atoi(val)
		if err != nil {
			gr.logger.Printf("Invalid version %s %v", val, err)
			return
		}
		gr.recoveredVer = append(gr.recoveredVer, v)
	}
}

// Fork a child and pass the listeners fd to it
func (gr *Graceful) Restart() (childPid int, err error) {
	var version []string

	version = append(
		version,
		strconv.Itoa(int(gr.conn.protocol.Major)),
		strconv.Itoa(int(gr.conn.protocol.Minor)),
	)
	ver_str := strings.Join(version, ",")

	// Pass fds to child using enviorments
	os.Setenv("_GRACEFUL_RESTART", ver_str)
	gr.logger.Printf("Start child with version: %v", ver_str)
	child := exec.Command(os.Args[0], os.Args[1:]...)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	child.ExtraFiles = []*os.File{gr.dev}
	err = child.Start()
	if err != nil {
		err = fmt.Errorf("Fork child error: %v", err)
		gr.logger.Print(err.Error())
		return 0, ErrGRestartFailed
	}
	childPid = child.Process.Pid
	childPath := gr.getChildPath(childPid)
	// Wait 60 seconds for the child getting ready
	for i := 0; i < gr.RestartTimeout; i++ {
		_, err = os.Stat(childPath)
		if !os.IsNotExist(err) {
			os.Remove(childPath)
			gr.logger.Printf("Process %d: child %d ok", os.Getpid(), childPid)
			// Child is ready
			return childPid, nil
		}
		time.Sleep(time.Second)
	}
	_, err = os.FindProcess(childPid)
	if err == nil { // Child process actually exists
		return childPid, ErrGRestartChildNotReady
	}
	return 0, ErrGRestartFailed
}

func (gr *Graceful) getChildPath(pid int) string {
	return filepath.Join(gr.runDir, fmt.Sprintf("%s_%d", gr.progName, pid))
}

// Register a callback for graceful restart.
// The callback should:
// 1) Call gr.Restart()
// 2) Close listeners
// 3) Wait for client connection to terminate and exit the program
// Before calling this, the caller should make listeners and get ready to serve request.
// After calling this, the caller should be in a loop.
func (gr *Graceful) Ready(sig os.Signal, restartFunc GRestartCallback) (err error) {
	err = WritePidFile(gr.runDir, gr.progName)
	if err != nil {
		return fmt.Errorf("Cannot create pidfile: %v", err)
	}
	if gr.recovered {
		childPath := gr.getChildPath(os.Getpid())
		f, err := os.OpenFile(childPath, os.O_WRONLY|os.O_CREATE, 0700)
		if err != nil {
			return fmt.Errorf("Cannot create %s: %v", childPath, err)
		}
		f.Close()
	}
	gr.restartCb = restartFunc
	go gr.watchDaemon()
	signal.Notify(gr.restartNoti, sig)
	gr.logger.Printf("Process %d ready", os.Getpid())
	return nil
}

func (gr *Graceful) watchDaemon() {
	var err error
	for {
		select {
		case <-gr.restartNoti:
			gr.lock.Lock()
			if gr.restarting {
				gr.lock.Unlock()
				continue
			} else {
				gr.restarting = true
				gr.lock.Unlock()
				gr.logger.Printf("Graceful restart triggered on pid=%d", os.Getpid())
				err = gr.restartCb()
				// restart Fail, do not exit
				if err == nil {
					return
				}
				gr.restarting = false
			}
		}
	}
}

func WritePidFile(runDir, progName string) (err error) {
	var f *os.File
	var fileLen int
	pidFile := filepath.Join(runDir, fmt.Sprintf("%s.pid", progName))
	f, err = os.OpenFile(pidFile, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err = f.Seek(0, os.SEEK_SET); err != nil {
		return
	}
	if fileLen, err = fmt.Fprint(f, os.Getpid()); err != nil {
		return err
	}
	if err = f.Truncate(int64(fileLen)); err != nil {
		return
	}
	if err = f.Sync(); err != nil {
		return
	}
	return nil
}
