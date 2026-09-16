package eventsourcing

import "runtime/debug"

// modulePath is this module's import path. It is used to find this module's
// own entry in the build information of a program that depends on it.
const modulePath = "github.com/terraskye/eventsourcing"

// unknownVersion is reported by [InstrumentationVersion] when the build
// information needed to determine the real version is unavailable.
const unknownVersion = "unknown"

// InstrumentationVersion is the version of this module, reported as the
// instrumentation version of the OpenTelemetry meter and tracer the otel
// subpackage creates.
//
// It is read once, at startup, from the build information the Go toolchain
// embeds in the consuming binary, so it always matches the version actually
// linked in and never has to be bumped by hand. Two values are reported
// instead of a release version:
//
//   - "(devel)" when this module is itself the main module, as it is when
//     running this repository's own tests.
//   - "unknown" when the build information is missing, or records no version
//     for this module — for example in a binary built with -buildvcs=false.
var InstrumentationVersion = instrumentationVersion()

// instrumentationVersion returns the version recorded for this module in the
// running binary's build information, or [unknownVersion] if there is none.
func instrumentationVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return unknownVersion
	}
	return versionFromBuildInfo(info)
}

// versionFromBuildInfo returns the version info records for this module,
// whether it is the main module or one of its dependencies, or
// [unknownVersion] if info does not mention it.
func versionFromBuildInfo(info *debug.BuildInfo) string {
	if info.Main.Path == modulePath {
		return moduleVersion(&info.Main)
	}
	for _, dep := range info.Deps {
		if dep.Path == modulePath {
			return moduleVersion(dep)
		}
	}
	return unknownVersion
}

// moduleVersion returns m's version, following any replace directives to the
// module that ultimately supplies the code, or [unknownVersion] if that
// module records no version.
func moduleVersion(m *debug.Module) string {
	for m.Replace != nil {
		m = m.Replace
	}
	if m.Version == "" {
		return unknownVersion
	}
	return m.Version
}
