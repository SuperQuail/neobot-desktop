//go:build linux

package main

import "testing"

func TestTrayInstalledRequiresRegistrationAndMenu(t *testing.T) {
	trayMu.Lock()
	broken, stopped, registered, menuReady := trayBroken, trayStopped, trayRegistered, trayMenuReady
	trayBroken, trayStopped, trayRegistered, trayMenuReady = false, false, false, false
	trayMu.Unlock()
	defer func() {
		trayMu.Lock()
		trayBroken, trayStopped, trayRegistered, trayMenuReady = broken, stopped, registered, menuReady
		trayMu.Unlock()
	}()

	if trayInstalled() {
		t.Fatal("tray installed before registration")
	}
	trayMu.Lock()
	trayRegistered, trayMenuReady = true, true
	trayMu.Unlock()
	if !trayInstalled() || !trayAvailable() {
		t.Fatal("registered tray was not reported as installed")
	}
	trayMu.Lock()
	trayStopped = true
	trayMu.Unlock()
	if trayInstalled() || trayAvailable() {
		t.Fatal("stopped tray was still reported as available")
	}
}
