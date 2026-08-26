//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"golang.org/x/sys/unix"
)

// mountKind records how an image got mounted, because the two differ in what
// they consume rather than in what they produce.
type mountKind string

const (
	mountFileBacked mountKind = "file-backed"
	mountLoop       mountKind = "loop"
)

// mountErofs mounts an erofs image read-only at target.
//
// Two paths, tried in this order:
//
//  1. File-backed, through the fsopen/fsconfig/fsmount/move_mount API. Needs
//     Linux 6.12+ with CONFIG_EROFS_FS_BACKED_BY_FILE. No loop device is
//     involved, so nothing caps how many images can be mounted at once.
//
//  2. Loop-backed, via mount(8). This is the fallback for kernels without that
//     option - including the OrbStack dev kernel, where the file-backed attempt
//     fails with EINVAL "block device required".
//
// The distinction matters operationally: mount(8) sets up a loop device even
// when the source is named as a plain file with no -o loop, and the number of
// loop devices is capped by the host's max_loop. A dev kernel capping it at 4
// is what makes the fallback worth keeping visible rather than silent.
//
// Both paths need CAP_SYS_ADMIN. File-backed removes the loop device, not the
// privilege - though it does mean the container needs SYS_ADMIN rather than
// full --privileged with /dev/loop-control.
func mountErofs(image, target string) (mountKind, error) {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return "", err
	}

	err := mountErofsFileBacked(image, target)
	if err == nil {
		return mountFileBacked, nil
	}
	// Only fall back when the kernel says it cannot take a plain file. ENOTBLK
	// ("block device required") is what a kernel without
	// CONFIG_EROFS_FS_BACKED_BY_FILE answers; EINVAL and ENOPROTOOPT cover
	// kernels too old to know the fs_context option at all. Anything else - a
	// missing image, a corrupt one, EPERM - would fail the same way on the
	// second path, and the first error is the informative one.
	if !errors.Is(err, unix.ENOTBLK) && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOPROTOOPT) {
		return "", fmt.Errorf("file-backed mount: %w", err)
	}

	out, lerr := exec.Command("mount", "-t", "erofs", "-o", "ro", image, target).CombinedOutput()
	if lerr != nil {
		return "", fmt.Errorf("file-backed mount unsupported (%v); loop mount failed: %s: %w", err, out, lerr)
	}
	return mountLoop, nil
}

func mountErofsFileBacked(image, target string) error {
	fd, err := unix.Fsopen("erofs", 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	if err := unix.FsconfigSetString(fd, "source", image); err != nil {
		return err
	}
	// EINVAL here is the "this kernel wants a block device" answer.
	if err := unix.FsconfigCreate(fd); err != nil {
		return err
	}

	mfd, err := unix.Fsmount(fd, 0, unix.MS_RDONLY)
	if err != nil {
		return err
	}
	defer unix.Close(mfd)

	return unix.MoveMount(mfd, "", unix.AT_FDCWD, target, unix.MOVE_MOUNT_F_EMPTY_PATH)
}

// unmountErofs detaches target. MNT_DETACH so a mount still being read comes
// away lazily instead of failing with EBUSY - the kernel keeps it alive until
// the last open file on it is closed.
func unmountErofs(target string) error {
	return unix.Unmount(target, unix.MNT_DETACH)
}
