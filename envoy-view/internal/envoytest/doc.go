// Package envoytest runs a real Envoy in a container and hands the resulting
// config dump to the code under test.
//
// Every fixture under internal/xds/testdata was originally hand-written from
// the ConfigDump proto definitions, which makes them a test of what we believe
// Envoy emits rather than of what it actually emits. Those are the same belief,
// so a wrong belief passes. This package closes that loop: a scenario declares
// an Envoy bootstrap, the framework boots the pinned Envoy image, waits for it
// to serve, and pulls /config_dump, so assertions run against ground truth.
//
// # Running
//
//	make test-container          # scenario tests, needs a container runtime
//	make fixtures                # regenerate internal/xds/testdata from Envoy
//
// Tests skip, rather than fail, when no container runtime is available, so
// "go test ./..." stays useful on a machine without one. Set
// ENVOY_TEST_REQUIRE=1 to turn that skip into a failure, which is what CI
// should do so a broken runtime is not silently green.
//
// # Environment
//
//	ENVOY_VERSION       Envoy release to run (default DefaultVersion)
//	ENVOY_IMAGE         full image reference, overriding ENVOY_VERSION
//	ENVOY_TEST_RUNTIME  container runtime to use: docker, podman, nerdctl
//	ENVOY_TEST_REQUIRE  fail instead of skipping when no runtime is found
//	ENVOY_TEST_KEEP     leave containers running after the test, for debugging
//	ENVOY_TEST_ONLINE   allow tests that reach the network (release checks)
//
// # Why files are copied rather than bind-mounted
//
// Scenario files are written into the container with "cp" instead of a bind
// mount. Bind mounts would be simpler, but filesystem xDS depends on inotify,
// and inotify events do not cross the host boundary reliably on macOS or on
// rootless podman. Copying puts the files on the container's own filesystem
// where the watcher behaves the same everywhere.
package envoytest
