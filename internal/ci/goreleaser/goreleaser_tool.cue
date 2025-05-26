package goreleaser

import (
	"encoding/yaml"
	"path"
	"strings"

	"tool/file"
	"tool/exec"
	"tool/os"
	"tool/cli"
)

command: release: {
	env: os.Environ

	let _env = env

	let _githubRef = *env.GITHUB_REF | "refs/no_ref_kind/not_a_release" // filled when running in CI
	let _githubRefName = path.Base(_githubRef)

	tempDir: file.MkdirTemp & {
		path: string
	}

	goMod: file.Create & {
		contents: "module mod.test"
		filename: path.Join([tempDir.path, "go.mod"])
	}

	latestCUE: exec.Run & {
		env: {
			_env

			GOPROXY: "direct" // skip proxy.golang.org in case its @latest is lagging behind
		}
		$after: goMod
		dir:    tempDir.path
		cmd: ["go", "list", "-m", "-f", "{{.Version}}", "cuelang.org/go@latest"]
		stdout: string
	}

	let latestCUEVersion = strings.TrimSpace(latestCUE.stdout)

	tidyUp: file.RemoveAll & {
		$after: latestCUE
		path:   tempDir.path
	}

	cueModRoot: exec.Run & {
		cmd: ["go", "list", "-m", "-f", "{{.Dir}}", "cuelang.org/go"]
		stdout: string
	}

	let goreleaserCmd = [
		"goreleaser", "release", "-f", "-", "--clean", "--snapshot",
	]
	let goreleaserConfigYAML = yaml.Marshal(config & {
		#latest: _githubRefName == strings.TrimSpace(latestCUE.stdout)
	})

	info: cli.Print & {
		text: """
			latest CUE version: \(latestCUEVersion)
			git ref: \(_githubRef)
			release name: \(_githubRefName)
			goreleaser cmd: \(strings.Join(goreleaserCmd, " "))

			goreleaser config yaml, indented for readability:
			  \(strings.Replace(goreleaserConfigYAML, "\n", "\n  ", -1))
			"""
	}

	goreleaser: exec.Run & {
		$after: info

		// Set the goreleaser configuration to be stdin
		stdin: goreleaserConfigYAML

		// Run at the root of the module
		dir: strings.TrimSpace(cueModRoot.stdout)

		cmd: goreleaserCmd
	}
}
