package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

var wg sync.WaitGroup
var runsvdir = "/usr/bin/runsvdir"
var serviceDir = "/etc/service"
var timeout = 300
var timeoutStr = "300"
var port int

const svbin = "/usr/bin/sv"

func init() {
	flag.StringVar(&runsvdir, "runsvdir", "/usr/bin/runsvdir", "path for runsvdir")
	flag.StringVar(&serviceDir, "servicedir", "/etc/service", "default dir where services are located")
	flag.IntVar(&timeout, "timeout", 300, "how long to wait for process to stop")
	flag.IntVar(&port, "log-port", 0, "udp port to listen for svlogd logs")
}

func main() {

	flag.Parse()
	fmt.Println("docker-runit-init")
	timeoutStr = fmt.Sprintf("%d", timeout)

	if port != 0 {
		fmt.Printf("starting udp listener for log messages")
		go startlogger(context.Background(), port)
	}

	if pid := os.Getpid(); pid != 1 {
		fmt.Printf("not running as pid 1. pid: %d\n", pid)
	}

	wg = sync.WaitGroup{}

	// Create a new context with a cancel function.
	// the context will be passed to listener(ctx) which will
	// be waiting for the context to be cancelled.
	// The cancel function will be listening to SIGTERM, SIGKILL
	// to begin shutting down runit services
	ctx, cancel := context.WithCancel(context.Background())
	runitCtx := listener(ctx)

	startRunit(runitCtx)

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGTERM, syscall.SIGKILL)

	go func() {
		for {
			s := <-c
			fmt.Printf("Singal '%s' recieved\n", s)
			cancel()
		}
	}()

	wg.Wait()

	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, 0, nil)
		if err != nil {
			fmt.Printf("reaped all child processes\n")
			break
		}
		fmt.Printf("reaped child process %d (WaitStatus: %+v)\n", pid, ws)
	}

}

// Listener will take in a context that's listening to the signal channel
// when the signal is triggered it will iterate through all the services
// and call "stop" with a longer timeout than the runit default.
// Listener also creates a new context with a cancel function so when its
// done stopping all the services it will call cancel on that context
// this context should be used to send SIGHUP to runsvdir to stop svrun
func listener(ctx context.Context) context.Context {

	services := getAllServices(serviceDir)
	for _, service := range services {
		fmt.Printf("registered to handle %s\n", service)
	}

	// create a __separate__ context so when all services are shutdown we can stop runit
	runitCtx, stopRunit := context.WithCancel(context.Background())
	wg.Add(1)
	go func() {

		lwg := sync.WaitGroup{}

		// block until the signal is triggered
		<-ctx.Done()
		fmt.Println("ctx/TERM rcvd called done")

		for _, service := range services {
			c := cmd(svbin, "-w", timeoutStr, "force-stop", service)
			fmt.Printf("stopping %s\n", service)
			lwg.Add(1)

			// Stop the service first and then its logger
			// if you stop them together the logger will finish before
			// and we could lose logs
			go func(service string, c *exec.Cmd) {
				defer lwg.Done()
				if err := c.Run(); err != nil {
					fmt.Printf("%s: %v\n", strings.Join(c.Args, " "), err)
				} else {
					fmt.Printf("stopped %s\n", service)
				}

				// TODO: add check
				logService := fmt.Sprintf("%s/log", service)
				stopLog := cmd(svbin, "-w", timeoutStr, "stop", logService)
				if err := stopLog.Run(); err != nil {
					fmt.Printf("%s: %v\n", strings.Join(stopLog.Args, " "), err)
				} else {
					fmt.Printf("stopped %s\n", logService)
				}
			}(service, c)

		}

		fmt.Println("waiting for jobs to stop")
		lwg.Wait()
		fmt.Println("all jobs stopped")

		// Now that all services have been stopped we can stop runit
		stopRunit()

		wg.Done()
		fmt.Println("listener end.")
	}()
	return runitCtx
}

// startRunit will start two goruntines, one to wait for runit
// and the second one listening when the context ends of the other jobs
// being stopped so we can close runit.
func startRunit(ctx context.Context) {

	// Instead of using exec.CommandContext we have to handle everything manually
	// because we want to send SIGHUP to sv
	c := cmd(runsvdir, serviceDir)
	wg.Add(1)

	go func() {

		if err := c.Start(); err != nil {
			panic(err)
		}
		fmt.Printf("runit started.")

		if err := c.Wait(); err != nil {
			if exiterr, ok := err.(*exec.ExitError); ok {
				// The program has exited with an exit code != 0
				// Status Code 111 is ok, its what runsvdir returns when it sends TERM to the children
				if status, ok := exiterr.Sys().(syscall.WaitStatus); ok && status.ExitStatus() != 111 {
					fmt.Printf("Exit Status: %d\n", status.ExitStatus())
				}
			} else {
				log.Fatalf("cmd.Wait: %v", err)
			}
		}

		fmt.Println("runit stopped.")
		wg.Done()
	}()

	go func() {
		<-ctx.Done()
		fmt.Printf("runit ctx done. sending HUP to runit\n")
		// send sighup so runsvdir sends a TERM to all its children
		c.Process.Signal(syscall.SIGHUP)
	}()

}

func getAllServices(serviceDir string) []string {

	services, err := filepath.Glob(filepath.Join(serviceDir, "*"))
	if err != nil {
		log.Fatal(err)
	}
	return services
}

func cmd(path string, args ...string) *exec.Cmd {
	return &exec.Cmd{
		Path:   path,
		Args:   append([]string{path}, args...),
		Env:    os.Environ(),
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
}
