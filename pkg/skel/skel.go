// Copyright 2014-2016 CNI authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package skel provides skeleton code for a CNI plugin.
// In particular, it implements argument parsing and validation.
package skel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/containernetworking/cni/pkg/ns"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/utils"
	"github.com/containernetworking/cni/pkg/version"
)

// CmdArgs captures all the arguments passed in to the plugin
// via both env vars and stdin
type CmdArgs struct {
	ContainerID   string
	Netns         string
	IfName        string
	Args          string
	Path          string
	NetnsOverride string
	StdinData     []byte
}

type dispatcher struct {
	Getenv func(string) string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	ConfVersionDecoder version.ConfigDecoder
	VersionReconciler  version.Reconciler
}

type reqForCmdEntry map[string]bool

func (t *dispatcher) getCmdArgsFromEnv() (string, *CmdArgs, *types.Error) {
	var cmd, contID, netns, ifName, args, path, netnsOverride string

	vars := []struct {
		name       string
		val        *string
		reqForCmd  reqForCmdEntry
		validateFn func(string) *types.Error
	}{
		{
			"CNI_COMMAND",
			&cmd,
			reqForCmdEntry{
				"ADD":   true,
				"CHECK": true,
				"DEL":   true,
				"GC":    true,
			},
			nil,
		},
		{
			"CNI_CONTAINERID",
			&contID,
			reqForCmdEntry{
				"ADD":   true,
				"CHECK": true,
				"DEL":   true,
			},
			utils.ValidateContainerID,
		},
		{
			"CNI_NETNS",
			&netns,
			reqForCmdEntry{
				"ADD":   true,
				"CHECK": true,
				"DEL":   false,
			},
			nil,
		},
		{
			"CNI_IFNAME",
			&ifName,
			reqForCmdEntry{
				"ADD":   true,
				"CHECK": true,
				"DEL":   true,
			},
			utils.ValidateInterfaceName,
		},
		{
			"CNI_ARGS",
			&args,
			reqForCmdEntry{
				"ADD":   false,
				"CHECK": false,
				"DEL":   false,
			},
			nil,
		},
		{
			"CNI_PATH",
			&path,
			reqForCmdEntry{
				"ADD":   true,
				"CHECK": true,
				"DEL":   true,
				"GC":    true,
			},
			nil,
		},
		{
			"CNI_NETNS_OVERRIDE",
			&netnsOverride,
			reqForCmdEntry{
				"ADD":   false,
				"CHECK": false,
				"DEL":   false,
			},
			nil,
		},
	}

	argsMissing := make([]string, 0)
	for _, v := range vars {
		*v.val = t.Getenv(v.name)
		if *v.val == "" {
			if v.reqForCmd[cmd] || v.name == "CNI_COMMAND" {
				argsMissing = append(argsMissing, v.name)
			}
		} else if v.reqForCmd[cmd] && v.validateFn != nil {
			if err := v.validateFn(*v.val); err != nil {
				return "", nil, err
			}
		}
	}

	if len(argsMissing) > 0 {
		joined := strings.Join(argsMissing, ",")
		return "", nil, types.NewError(types.ErrInvalidEnvironmentVariables, fmt.Sprintf("required env variables [%s] missing", joined), "")
	}

	if cmd == "VERSION" {
		t.Stdin = bytes.NewReader(nil)
	}

	stdinData, err := io.ReadAll(t.Stdin)
	if err != nil {
		return "", nil, types.NewError(types.ErrIOFailure, fmt.Sprintf("error reading from stdin: %v", err), "")
	}

	if cmd != "VERSION" {
		if err := validateConfig(stdinData); err != nil {
			return "", nil, err
		}
	}

	cmdArgs := &CmdArgs{
		ContainerID:   contID,
		Netns:         netns,
		IfName:        ifName,
		Args:          args,
		Path:          path,
		StdinData:     stdinData,
		NetnsOverride: netnsOverride,
	}
	return cmd, cmdArgs, nil
}

func (t *dispatcher) checkVersionAndCall(cmdArgs *CmdArgs, pluginVersionInfo version.PluginInfo, toCall func(*CmdArgs) error) *types.Error {
	configVersion, err := t.ConfVersionDecoder.Decode(cmdArgs.StdinData)
	if err != nil {
		return types.NewError(types.ErrDecodingFailure, err.Error(), "")
	}
	verErr := t.VersionReconciler.Check(configVersion, pluginVersionInfo)
	if verErr != nil {
		return types.NewError(types.ErrIncompatibleCNIVersion, "incompatible CNI versions", verErr.Details())
	}

	if toCall == nil {
		return nil
	}

	if err = toCall(cmdArgs); err != nil {
		var e *types.Error
		if errors.As(err, &e) {
			// don't wrap Error in Error
			return e
		}
		return types.NewError(types.ErrInternal, err.Error(), "")
	}

	return nil
}

func validateConfig(jsonBytes []byte) *types.Error {
	var conf struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(jsonBytes, &conf); err != nil {
		return types.NewError(types.ErrDecodingFailure, fmt.Sprintf("error unmarshall network config: %v", err), "")
	}
	if conf.Name == "" {
		return types.NewError(types.ErrInvalidNetworkConfig, "missing network name", "")
	}
	if err := utils.ValidateNetworkName(conf.Name); err != nil {
		return err
	}
	return nil
}

func (t *dispatcher) pluginMain(funcs CNIFuncs, versionInfo version.PluginInfo, about string) *types.Error {
	cmd, cmdArgs, err := t.getCmdArgsFromEnv()
	if err != nil {
		// Print the about string to stderr when no command is set
		if err.Code == types.ErrInvalidEnvironmentVariables && t.Getenv("CNI_COMMAND") == "" && about != "" {
			_, _ = fmt.Fprintln(t.Stderr, about)
			_, _ = fmt.Fprintf(t.Stderr, "CNI protocol versions supported: %s\n", strings.Join(versionInfo.SupportedVersions(), ", "))
			return nil
		}
		return err
	}

	switch cmd {
	case "ADD":
		err = t.checkVersionAndCall(cmdArgs, versionInfo, funcs.Add)
		if err != nil {
			return err
		}
		if strings.ToUpper(cmdArgs.NetnsOverride) != "TRUE" && cmdArgs.NetnsOverride != "1" {
			isPluginNetNS, checkErr := ns.CheckNetNS(cmdArgs.Netns)
			if checkErr != nil {
				return checkErr
			} else if isPluginNetNS {
				return types.NewError(types.ErrInvalidNetNS, "plugin's netns and netns from CNI_NETNS should not be the same", "")
			}
		}
	case "CHECK":
		configVersion, err := t.ConfVersionDecoder.Decode(cmdArgs.StdinData)
		if err != nil {
			return types.NewError(types.ErrDecodingFailure, err.Error(), "")
		}
		if gtet, err := version.GreaterThanOrEqualTo(configVersion, "0.4.0"); err != nil {
			return types.NewError(types.ErrDecodingFailure, err.Error(), "")
		} else if !gtet {
			return types.NewError(types.ErrIncompatibleCNIVersion, "config version does not allow CHECK", "")
		}
		for _, pluginVersion := range versionInfo.SupportedVersions() {
			gtet, err := version.GreaterThanOrEqualTo(pluginVersion, configVersion)
			if err != nil {
				return types.NewError(types.ErrDecodingFailure, err.Error(), "")
			} else if gtet {
				if err := t.checkVersionAndCall(cmdArgs, versionInfo, funcs.Check); err != nil {
					return err
				}
				return nil
			}
		}
		return types.NewError(types.ErrIncompatibleCNIVersion, "plugin version does not allow CHECK", "")
	case "DEL":
		err = t.checkVersionAndCall(cmdArgs, versionInfo, funcs.Del)
		if err != nil {
			return err
		}
		if strings.ToUpper(cmdArgs.NetnsOverride) != "TRUE" && cmdArgs.NetnsOverride != "1" {
			isPluginNetNS, checkErr := ns.CheckNetNS(cmdArgs.Netns)
			if checkErr != nil {
				return checkErr
			} else if isPluginNetNS {
				return types.NewError(types.ErrInvalidNetNS, "plugin's netns and netns from CNI_NETNS should not be the same", "")
			}
		}
	case "GC":
		configVersion, err := t.ConfVersionDecoder.Decode(cmdArgs.StdinData)
		if err != nil {
			return types.NewError(types.ErrDecodingFailure, err.Error(), "")
		}
		if gtet, err := version.GreaterThanOrEqualTo(configVersion, "1.1.0"); err != nil {
			return types.NewError(types.ErrDecodingFailure, err.Error(), "")
		} else if !gtet {
			return types.NewError(types.ErrIncompatibleCNIVersion, "config version does not allow GC", "")
		}
		for _, pluginVersion := range versionInfo.SupportedVersions() {
			gtet, err := version.GreaterThanOrEqualTo(pluginVersion, configVersion)
			if err != nil {
				return types.NewError(types.ErrDecodingFailure, err.Error(), "")
			} else if gtet {
				if err := t.checkVersionAndCall(cmdArgs, versionInfo, funcs.GC); err != nil {
					return err
				}
				return nil
			}
		}
		return types.NewError(types.ErrIncompatibleCNIVersion, "plugin version does not allow GC", "")
	case "VERSION":
		if err := versionInfo.Encode(t.Stdout); err != nil {
			return types.NewError(types.ErrIOFailure, err.Error(), "")
		}
	default:
		return types.NewError(types.ErrInvalidEnvironmentVariables, fmt.Sprintf("unknown CNI_COMMAND: %v", cmd), "")
	}

	return err
}

// PluginMainWithError is the core "main" for a plugin. It accepts
// callback functions for add, check, and del CNI commands and returns an error.
//
// The caller must also specify what CNI spec versions the plugin supports.
//
// It is the responsibility of the caller to check for non-nil error return.
//
// For a plugin to comply with the CNI spec, it must print any error to stdout
// as JSON and then exit with nonzero status code.
//
// To let this package automatically handle errors and call os.Exit(1) for you,
// use PluginMain() instead.
//
// Deprecated: Use github.com/containernetworking/cni/pkg/skel.PluginMainFuncsWithError instead.
func PluginMainWithError(cmdAdd, cmdCheck, cmdDel func(_ *CmdArgs) error, versionInfo version.PluginInfo, about string) *types.Error {
	return PluginMainFuncsWithError(CNIFuncs{Add: cmdAdd, Check: cmdCheck, Del: cmdDel}, versionInfo, about)
}

// CNIFuncs contains a group of callback command funcs to be passed in as
// parameters to the core "main" for a plugin.
type CNIFuncs struct {
	Add   func(_ *CmdArgs) error
	Del   func(_ *CmdArgs) error
	Check func(_ *CmdArgs) error
	GC    func(_ *CmdArgs) error
}

// PluginMainFuncsWithError is the core "main" for a plugin. It accepts
// callback functions defined within CNIFuncs and returns an error.
//
// The caller must also specify what CNI spec versions the plugin supports.
//
// It is the responsibility of the caller to check for non-nil error return.
//
// For a plugin to comply with the CNI spec, it must print any error to stdout
// as JSON and then exit with nonzero status code.
//
// To let this package automatically handle errors and call os.Exit(1) for you,
// use PluginMainFuncs() instead.
func PluginMainFuncsWithError(funcs CNIFuncs, versionInfo version.PluginInfo, about string) *types.Error {
	return (&dispatcher{
		Getenv: os.Getenv,
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}).pluginMain(funcs, versionInfo, about)
}

// PluginMainFuncs is the core "main" for a plugin which includes automatic error handling.
// This is a newer alternative func to PluginMain which abstracts CNI commands within a
// CNIFuncs interface.
//
// The caller must also specify what CNI spec versions the plugin supports.
//
// The caller can specify an "about" string, which is printed on stderr
// when no CNI_COMMAND is specified. The recommended output is "CNI plugin <foo> v<version>"
//
// When an error occurs in any func in CNIFuncs, PluginMainFuncs will print the error
// as JSON to stdout and call os.Exit(1).
//
// To have more control over error handling, use PluginMainFuncsWithError() instead.
func PluginMainFuncs(funcs CNIFuncs, versionInfo version.PluginInfo, about string) {
	if e := PluginMainFuncsWithError(funcs, versionInfo, about); e != nil {
		if err := e.Print(); err != nil {
			log.Print("Error writing error JSON to stdout: ", err)
		}
		os.Exit(1)
	}
}

// PluginMain is the core "main" for a plugin which includes automatic error handling.
//
// The caller must also specify what CNI spec versions the plugin supports.
//
// The caller can specify an "about" string, which is printed on stderr
// when no CNI_COMMAND is specified. The recommended output is "CNI plugin <foo> v<version>"
//
// When an error occurs in either cmdAdd, cmdCheck, or cmdDel, PluginMain will print the error
// as JSON to stdout and call os.Exit(1).
//
// To have more control over error handling, use PluginMainWithError() instead.
//
// Deprecated: Use github.com/containernetworking/cni/pkg/skel.PluginMainFuncs instead.
func PluginMain(cmdAdd, cmdCheck, cmdDel func(_ *CmdArgs) error, versionInfo version.PluginInfo, about string) {
	if e := PluginMainWithError(cmdAdd, cmdCheck, cmdDel, versionInfo, about); e != nil {
		if err := e.Print(); err != nil {
			log.Print("Error writing error JSON to stdout: ", err)
		}
		os.Exit(1)
	}
}
