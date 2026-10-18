package fuse

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
)

const (
	errNoChildProcesses = "wait: no child processes"
)

func handleFusermountStderr(errCh chan<- error) func(line string) (ignore bool) {
	return func(line string) (ignore bool) {
		if line == `fusermount3: failed to open /etc/fuse.conf: Permission denied` {
			// Silence this particular message, it occurs way too
			// commonly and isn't very relevant to whether the mount
			// succeeds or not.
			return true
		}

		const (
			noMountpointPrefix = `fusermount3: failed to access mountpoint `
			noMountpointSuffix = `: No such file or directory`
		)
		if strings.HasPrefix(line, noMountpointPrefix) && strings.HasSuffix(line, noMountpointSuffix) {
			// re-extract it from the error message in case some layer
			// changed the path
			mountpoint := line[len(noMountpointPrefix) : len(line)-len(noMountpointSuffix)]
			err := &MountpointDoesNotExistError{
				Path: mountpoint,
			}
			select {
			case errCh <- err:
				return true
			default:
				// not the first error; fall back to logging it
				return false
			}
		}

		return false
	}
}

// isBoringFusermountError returns whether the Wait error is
// uninteresting; exit status 1 is.
func isBoringFusermountError(err error) bool {
	if err, ok := err.(*exec.ExitError); ok && err.Exited() {
		if status, ok := err.Sys().(syscall.WaitStatus); ok && status.ExitStatus() == 1 {
			return true
		}
	}
	return false
}

func mount(dir string, conf *mountConfig) (fusefd *os.File, err error) {
	// When running as root we can call mount(2) directly and skip the
	// fusermount3 helper. This removes the runtime dependency on the
	// fusermount3 binary for privileged daemons.
	if os.Geteuid() == 0 {
		return directMount(dir, conf)
	}

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("socketpair error: %v", err)
	}

	writeFile := os.NewFile(uintptr(fds[0]), "fusermount-child-writes")
	defer writeFile.Close()

	readFile := os.NewFile(uintptr(fds[1]), "fusermount-parent-reads")
	defer readFile.Close()

	cmd := exec.Command(
		"fusermount3",
		"-o", conf.getOptions(),
		"--",
		dir,
	)
	cmd.Env = append(os.Environ(), "_FUSE_COMMFD=3")

	cmd.ExtraFiles = []*os.File{writeFile}

	var wg sync.WaitGroup
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("setting up fusermount stderr: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("setting up fusermount stderr: %v", err)
	}

	if err := cmd.Start(); err != nil {
		// If the process succeeded, don't consider "no child process" as an error;
		// see https://github.com/kubernetes/kubernetes/issues/103753.
		if !(err.Error() == errNoChildProcesses && cmd.ProcessState.Success()) {
			return nil, fmt.Errorf("fusermount: %v", err)
		}
	}

	helperErrCh := make(chan error, 1)
	wg.Add(2)
	go lineLogger(&wg, "mount helper output", neverIgnoreLine, stdout)
	go lineLogger(&wg, "mount helper error", handleFusermountStderr(helperErrCh), stderr)
	wg.Wait()
	if err := cmd.Wait(); err != nil {
		// see if we have a better error to report
		select {
		case helperErr := <-helperErrCh:
			// log the Wait error if it's not what we expected
			if !isBoringFusermountError(err) {
				log.Printf("mount helper failed: %v", err)
			}
			// and now return what we grabbed from stderr as the real
			// error
			return nil, helperErr
		default:
			// nope, fall back to generic message
		}

		return nil, fmt.Errorf("fusermount: %v", err)
	}

	c, err := net.FileConn(readFile)
	if err != nil {
		return nil, fmt.Errorf("FileConn from fusermount socket: %v", err)
	}
	defer c.Close()

	uc, ok := c.(*net.UnixConn)
	if !ok {
		return nil, fmt.Errorf("unexpected FileConn type; expected UnixConn, got %T", c)
	}

	buf := make([]byte, 32) // expect 1 byte
	oob := make([]byte, 32) // expect 24 bytes
	_, oobn, _, _, err := uc.ReadMsgUnix(buf, oob)
	scms, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, fmt.Errorf("ParseSocketControlMessage: %v", err)
	}
	if len(scms) != 1 {
		return nil, fmt.Errorf("expected 1 SocketControlMessage; got scms = %#v", scms)
	}
	scm := scms[0]
	gotFds, err := syscall.ParseUnixRights(&scm)
	if err != nil {
		return nil, fmt.Errorf("syscall.ParseUnixRights: %v", err)
	}
	if len(gotFds) != 1 {
		return nil, fmt.Errorf("wanted 1 fd; got %#v", gotFds)
	}
	f := os.NewFile(uintptr(gotFds[0]), "/dev/fuse")
	return f, nil
}

// kernelMountOptions is the set of options accepted by the kernel FUSE
// driver and passed through in the mount(2) data string.
var kernelMountOptions = map[string]struct{}{
	"allow_other":         {},
	"default_permissions": {},
	"max_read":            {},
	"blksize":             {},
}

// mountFlagOptions maps options that fusermount3 turns into mount(2)
// flags (see mount_flags in libfuse util/fusermount.c). set=true adds
// the flag, set=false clears it.
var mountFlagOptions = map[string]struct {
	flag uintptr
	set  bool
}{
	"ro":   {syscall.MS_RDONLY, true},
	"dev":  {syscall.MS_NODEV, false},
	"suid": {syscall.MS_NOSUID, false},
}

// directMountParams holds the mount(2) arguments derived from a
// mountConfig, excluding the per-mount fd and rootmode.
type directMountParams struct {
	source string
	fstype string
	flags  uintptr
	data   []string
}

// getDirectMountParams translates conf.options into mount(2) arguments
// the way fusermount3 does. Any option it cannot honor is an error, so
// the direct path fails closed instead of silently producing a mount
// with weaker semantics than the fusermount3 path would.
func getDirectMountParams(conf *mountConfig) (*directMountParams, error) {
	p := &directMountParams{
		source: "fuse",
		fstype: "fuse",
		flags:  syscall.MS_NOSUID | syscall.MS_NODEV,
	}

	keys := make([]string, 0, len(conf.options))
	for k := range conf.options {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var unsupported []string
	for _, k := range keys {
		v := conf.options[k]
		switch k {
		case "fsname":
			if v != "" {
				p.source = v
			}
			continue
		case "subtype":
			if v != "" {
				p.fstype = "fuse." + v
			}
			continue
		}
		if mf, ok := mountFlagOptions[k]; ok && v == "" {
			if mf.set {
				p.flags |= mf.flag
			} else {
				p.flags &^= mf.flag
			}
			continue
		}
		if _, ok := kernelMountOptions[k]; ok {
			if v == "" {
				p.data = append(p.data, k)
			} else {
				p.data = append(p.data, k+"="+v)
			}
			continue
		}
		unsupported = append(unsupported, k)
	}
	if len(unsupported) > 0 {
		return nil, fmt.Errorf("mount options not supported when mounting directly as root: %s", strings.Join(unsupported, ", "))
	}
	return p, nil
}

func directMount(dir string, conf *mountConfig) (*os.File, error) {
	p, err := getDirectMountParams(conf)
	if err != nil {
		return nil, err
	}

	f, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0o000)
	if err != nil {
		return nil, err
	}

	var st syscall.Stat_t
	if err := syscall.Stat(dir, &st); err != nil {
		f.Close()
		return nil, fmt.Errorf("stat %s: %v", dir, err)
	}
	rootmode := st.Mode & syscall.S_IFMT

	parts := []string{
		fmt.Sprintf("fd=%d", f.Fd()),
		fmt.Sprintf("rootmode=%o", rootmode),
		fmt.Sprintf("user_id=%d", os.Getuid()),
		fmt.Sprintf("group_id=%d", os.Getgid()),
	}
	parts = append(parts, p.data...)
	opts := strings.Join(parts, ",")

	if err := syscall.Mount(p.source, dir, p.fstype, p.flags, opts); err != nil {
		f.Close()
		return nil, fmt.Errorf("fuse mount at %s: %v", dir, err)
	}

	return f, nil
}
