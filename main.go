package main

import (
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"gopkg.in/lxc/go-lxc.v2"
)

var (
	lxcpath string
	containerName string
)
func init() {
	flag.StringVar(&lxcpath, "lxcpath", lxc.DefaultConfigPath(), "the path to the container roots")
	flag.StringVar(&containerName, "container-name", "precise", "the container to start up")
	flag.Parse()
}

// Produces the pid of the supplied pid's parent.
func ParentPid(pid int) (int, error) {
	cmd := exec.Command("sudo", "/bin/bash", "-c", fmt.Sprintf("cat /proc/%d/status | grep PPid | awk '{ print $2 }'", pid))
	
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}

	if err := cmd.Start(); err != nil {
		return 0, err
	}

	b, err := ioutil.ReadAll(stdoutPipe)
	if err != nil {
		return 0, err
	}

	if err := cmd.Wait(); err != nil {
		return 0, err
	}

	i, err := strconv.Atoi(strings.Replace(string(b), "\n","",-1))
	if err != nil {
		return 0, err
	}
	return i, nil
}

// Produces all the FIFO file descriptors from a given pid.
func FifoInodes(pid int) ([]int, error) {
	cmd := exec.Command("sudo", "/bin/bash", "-c", fmt.Sprintf("lsof -p %d 2>/dev/null | awk '$5 == \"FIFO\" {print $8}'", pid))

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return []int{}, err
	}

	if err := cmd.Start(); err != nil {
		return []int{}, err
	}

	b, err := ioutil.ReadAll(stdoutPipe)
	if err != nil {
		return []int{}, err
	}

	if err := cmd.Wait(); err != nil {
		return []int{}, err
	}

	var inodes []int
	for _, s := range(strings.Split(string(b), "\n")) {
		if s == "" {
			continue
		}
		i, err := strconv.Atoi(s)
		if err != nil {
			return []int{}, err
		}
		inodes = append(inodes, i)
	}
	return inodes, nil
}

// Pulls all the FIFO file descriptors out of the go process that execed the container's init process and 
// cross-reference them with the ones that are in this go process.  If we have found any overlap, then 
// this process failed to CLOEXEC correctly and the init process has inherited a file descriptor that it 
// should not have.
//
// Produces all inodes shared by this process and the container.
func fifoIntersection(c *lxc.Container) ([]int, error) {
	// Given the following PID tree:
	// 
	// vagrant  10676  0.0  0.0 144828  3476 ?        Ss   23:02   0:00 /tmp/go-build491052279/command-line-arguments/_obj/exe/main -stderrthreshold=info
	// 100000   10711  0.1  0.0  24184  3084 ?        Ss   23:02   0:00  \_ /sbin/init
	// 100000   10910  0.0  0.0  17240   168 ?        S    23:02   0:00      \_ upstart-udev-bridge --daemon
	// 100000   10919  0.0  0.0  21340  2300 ?        Ss   23:02   0:00      \_ /sbin/udevd --daemon
	// 
	// `c.InitPid` would produce "10711" but we want the parent, "10676", which the C-side of the go-lxc library forks.
	parentPid, err := ParentPid(c.InitPid())
	if err != nil {
		return []int{}, fmt.Errorf("Can't get container's parent's PID's inodes: %v", err)
	}

	cInodes, err := FifoInodes(parentPid)
	if err != nil {
		return []int{}, fmt.Errorf("Can't get container parent's PID's inodes: %v", err)
	}
	goInodes, err := FifoInodes(os.Getpid())
	if err != nil {
		return []int{}, fmt.Errorf("Can't get Go proc's FIFO inodes: %v", err)
	}

	//fmt.Printf("ci: %v gi: %v\n", cInodes, goInodes)

	goInodesMap := make(map[int]struct{})
	for _, ci := range(goInodes) {
		goInodesMap[ci] = struct{}{}
	}
	
	var isect []int
	for _, ci := range(cInodes) {
		if _, ok := goInodesMap[ci]; ok {
			isect = append(isect, ci)
		}
	}

	return isect, nil
}

// Start up the container, while simulatenously opening a bunch of file descriptors.  After
// startup, see if any of those file descriptors ended up getting inherited accidentally into the
// process that forked() to exec the container's init.
//
// Produces whether the race was detected, or if an internal error occurred.
func attemptRace() (bool, error) {
	fmt.Printf("Attempting race with starting go runtime pid %d\n", os.Getpid())

	c, err := lxc.NewContainer(containerName, lxcpath)
	if err != nil {
		return false, fmt.Errorf("[%s] Can't create container: %v", c.Name(), err)
	}

	var fifos []io.Closer
	terminate := make(chan bool)

	// Kick off a goroutine to spin and create a whole bunch of FIFOs...
	go func(terminate chan bool) {
		for {
			select {
			case <-terminate:
				return
			default:
				r, w, _ := os.Pipe()
				fifos = append(fifos, r)
				fifos = append(fifos, w)
				time.Sleep(10 * time.Millisecond)
			}
		}
	}(terminate)

	// ...while simulateously starting a container up, which will call into liblxc and fork()/exec()
	// /sbin/init for the newly-started up container.
	err = c.Start()
	terminate <- true

	if err != nil {
		return false, fmt.Errorf("[%s] Can't start container: %v", c.Name(), err)
	} else {
		ppid, _ := ParentPid(c.InitPid())
		fmt.Printf("Started %s (init pid: %d; parent pid: %d)\n", c.Name(), c.InitPid(), ppid)
	}
	defer func() {
		c.Stop()
		for _, file := range(fifos) {
			file.Close()
		}
	}()

	// We should not see any file descriptors created in the above goroutine; if we do, there is
	// a race to set CLOEXEC on the file descriptors created in the goroutine and fork()ing a child
	// process to spawn init.
	inodes, err := fifoIntersection(c)
	if err != nil {
		return false, err
	}
	if len(inodes) != 0 {
		fmt.Printf("Found the following intersecting inodes: %v\n", inodes)
		return true, nil
	}

	return false, nil
}

// The entry point.  Returns when a race is observed or an internal error is found.
func main() {
	cnt := 1
	for {
		raceDetected, err := attemptRace()
		if err != nil {
			fmt.Printf("Error whilst attempting to reproduce the race: %v\n", err)
			os.Exit(1)
		}
		if raceDetected {
			fmt.Printf("*** inode race detected after %d attempts\n", cnt)
			os.Exit(0)
		}
		cnt++
	}
}

