package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"glasshouse/internal/backend"
	"glasshouse/internal/session"
)

// cmdStart boots a stopped session again from the disk that `glasshouse run --keep` left behind, so what was installed
// or changed inside the VM is still there. The session keeps the policy it was created with.
func cmdStart(args []string) (err error) {
	args, command := splitDashDash(args)
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: glasshouse start <session> [-- command...]")
	}
	home, err := sessionHome()
	if err != nil {
		return err
	}
	in, err := session.Load(home, fs.Arg(0))
	if err != nil {
		return err
	}
	be, err := backendByName(in.Backend)
	if err != nil {
		return err
	}
	spec := specFromInfo(home, in, be)
	if err := checkResumable(in, spec); err != nil {
		return err
	}
	pol, err := loadSessionPolicy(home, in.ID)
	if err != nil {
		return err
	}
	if err := be.Preflight(&spec); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "verifying base image...")
	if err := session.VerifyImage(spec.Base); err != nil {
		return err
	}
	sha, err := session.ImageSHA(spec.Base)
	if err != nil {
		return err
	}
	if in.BaseSHA256 == "" || sha != in.BaseSHA256 {
		return fmt.Errorf("the base image has been rebuilt since session %s was created; its kept disk is layered on the old image and can no longer be booted (remove it with glasshouse rm %s)", in.ID, in.ID)
	}

	// a dead session left its sockets, key and seed behind: clear them, keep the disk (this is what a clean stop does)
	cleanup(session.Dir(home, in.ID), true)
	prev := in
	launched := false
	defer func() {
		if !launched { // nothing started: leave the session as it was, the disk is untouched
			_ = session.Save(home, prev)
		}
	}()
	token, err := session.NewToken()
	if err != nil {
		return err
	}
	in.Boots++
	in.Token, in.State, in.Error, in.Supervisor = token, "creating", "", 0
	in.SSHPort, in.ProxyAddr, in.GatewayAddr = 0, "", ""
	// the key and token of the previous run were deleted at stop; a fresh pair goes in through a new cloud-init instance
	if err := writeSeed(be, spec, pol, token, in.Boots); err != nil {
		return err
	}
	if err := session.Save(home, in); err != nil {
		return err
	}
	mode := modeShell
	if in.GUI {
		mode = modeGUI
	}
	return launchSession(home, be, in, pol, mode, command, true, func() { launched = true })
}

// checkResumable says whether a session has a kept disk that can be started again: it is stopped, failed, or dead
// (its supervisor is gone without having cleaned up, e.g. after a host reboot).
func checkResumable(in session.Info, spec backend.Spec) error {
	switch {
	case in.Alive():
		return fmt.Errorf("session %s is already running", in.ID)
	case !in.Persistent:
		return fmt.Errorf("session %s was started without --keep, so its disk was discarded when it stopped; start a new one with glasshouse run --keep", in.ID)
	case in.State == "stopped" || in.State == "failed":
	case (in.State == "ready" || in.State == "starting") && !in.Alive():
		// "dead": the supervisor vanished (host reboot, kill -9) without cleaning up; the disk is still there
	default:
		return fmt.Errorf("session %s is %s; only a stopped session can be started", in.ID, in.State)
	}
	if _, err := os.Stat(spec.Disk); err != nil {
		return fmt.Errorf("session %s has no disk left (%s)", in.ID, filepath.Base(spec.Disk))
	}
	return nil
}
