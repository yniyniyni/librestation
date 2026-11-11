package main

import "testing"

// androidToolEnv must supply ANDROID_ROOT and ANDROID_DATA so the `input`
// helper's app_process does not abort in ICU setup. voiced is launched by
// mysudo without them, which otherwise drops every forwarded remote key.
func TestAndroidToolEnvSuppliesICUVars(t *testing.T) {
	t.Setenv("ANDROID_ROOT", "")
	t.Setenv("ANDROID_DATA", "")
	env := androidToolEnv()
	for _, want := range []string{"ANDROID_ROOT=/system", "ANDROID_DATA=/data"} {
		if !contains(env, want) {
			t.Errorf("androidToolEnv() missing %q; got %v", want, env)
		}
	}
}

// An inherited value is kept rather than overridden.
func TestAndroidToolEnvKeepsInherited(t *testing.T) {
	t.Setenv("ANDROID_ROOT", "/system_root")
	t.Setenv("ANDROID_DATA", "/data")
	env := androidToolEnv()
	if contains(env, "ANDROID_ROOT=/system") {
		t.Errorf("androidToolEnv() overrode an inherited ANDROID_ROOT: %v", env)
	}
	if !contains(env, "ANDROID_ROOT=/system_root") {
		t.Errorf("androidToolEnv() dropped the inherited ANDROID_ROOT: %v", env)
	}
}

func contains(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}
