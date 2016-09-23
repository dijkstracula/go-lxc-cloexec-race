This is a simple test to validate that `go-lxc` does not correctly prevent
file descriptors from being inherited while we concurrently fork() processes
to perform container operations.  This issue has been [filed](https://github.com/lxc/go-lxc/issues/66) with the go-lxc
maintainers.

=== Description

go-lxc has a classic CLOEXEC race where the underlying liblxc C library will,
when we wish to start a new container, fork() and exec() the /sbin/init process,
but does not ensure that those children do not inherit file descriptors that have
been concurrently opened in the parent process.  As a result, because the inode
reference count will be higher than we expect, application code cannot assume
that close()ing a fd will behave the way we expect it to - in particular, closing
the write half of a pipe may not also close the read half of the pipe, leading to
arbitrarily long hangs.

In contrast, when the Go runtime wants to spawn a child process (e.g. in os/exec),
it takes a [lock](https://golang.org/src/syscall/exec_unix.go) to ensure mutual
exclusion.

We observed the aforementioned pipe hang on an internal project and this repo is
intended to help reproduce the behaviour.

=== Example

```
Attempting race with starting go runtime pid 20297
Started precise (init pid: 20342; parent pid: 20306)
Found the following intersecting inodes: [505657 505657]
*** inode race detected after 1 attempts
```

We can confirm this with manual `lsof`ing:

```
➜  $ sudo lsof -p 20306 | grep 505657
main    20306 vagrant    4r  FIFO               0,10      0t0     505657 pipe
main    20306 vagrant    5w  FIFO               0,10      0t0     505657 pipe
➜  $ sudo lsof -p 20297 | grep 505657
main    20297 vagrant    4r  FIFO   0,10      0t0  505657 pipe
main    20297 vagrant    5w  FIFO   0,10      0t0  505657 pipe
```

The relevant bits of the process tree:
```
vagrant   1776  0.0  0.2  50392 11752 pts/0    Ss   14:38   0:01  |       \_ /bin/zsh -l
vagrant  20274  1.2  0.2 127188 11352 pts/0    Sl+  23:45   0:00  |           \_ go run main.go
vagrant  20297  0.0  0.1 136304  4844 pts/0    Sl+  23:45   0:00  |               \_ /tmp/go-build525531552/command-line-arguments/_obj/exe/main
...
vagrant  20306  0.0  0.0 144732  3932 ?        Ss   23:45   0:00 /tmp/go-build525531552/command-line-arguments/_obj/exe/main
100000   20342  1.0  0.0  24072  3148 ?        Ss   23:45   0:00  \_ /sbin/init
100000   20523  0.0  0.0  15196   184 ?        S    23:45   0:00      \_ upstart-socket-bridge --daemon
100000   20541  0.0  0.0  17240   168 ?        S    23:45   0:00      \_ upstart-udev-bridge --daemon
```

=== Suggested fixes

Because the fork() happens in C-land, I'm not sure of a completely clean solution.
One thing that comes to mind that might work is a double-fork, where we let the go
runtime fork first, to ensure that no concurrent fd creation could have been inherited,
and then we let liblxc do its thing.  I don't know if such a low-level primitive is
exposed up into golang "userspace", though.
