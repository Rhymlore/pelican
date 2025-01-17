/***************************************************************
*
* Copyright (C) 2023, Pelican Project, Morgridge Institute for Research
*
* Licensed under the Apache License, Version 2.0 (the "License"); you
* may not use this file except in compliance with the License.  You may
* obtain a copy of the License at
*
*    http://www.apache.org/licenses/LICENSE-2.0
*
* Unless required by applicable law or agreed to in writing, software
* distributed under the License is distributed on an "AS IS" BASIS,
* WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
* See the License for the specific language governing permissions and
* limitations under the License.
*
***************************************************************/

package main

import (
	"os"

	"github.com/pelicanplatform/pelican/client"
	"github.com/pelicanplatform/pelican/config"
	"github.com/pelicanplatform/pelican/param"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var (
	putCmd = &cobra.Command{
		Use:   "put {source ...} {destination}",
		Short: "Send a file to a Pelican federation",
		Run:   putMain,
	}
)

func init() {
	flagSet := putCmd.Flags()
	flagSet.StringP("token", "t", "", "Token file to use for transfer")
	flagSet.BoolP("recursive", "r", false, "Recursively upload a directory.  Forces methods to only be http to get the freshest directory contents")
	objectCmd.AddCommand(putCmd)
}

func putMain(cmd *cobra.Command, args []string) {

	client.ObjectClientOptions.Version = version

	err := config.InitClient()
	if err != nil {
		log.Errorln(err)
		os.Exit(1)
	}

	// Set the progress bars to the command line option
	client.ObjectClientOptions.Token, _ = cmd.Flags().GetString("token")

	// Check if the program was executed from a terminal
	// https://rosettacode.org/wiki/Check_output_device_is_a_terminal#Go
	if fileInfo, _ := os.Stdout.Stat(); (fileInfo.Mode()&os.ModeCharDevice) != 0 && param.Logging_LogLocation.GetString() == "" && !param.Logging_DisableProgressBars.GetBool() {
		client.ObjectClientOptions.ProgressBars = true
	} else {
		client.ObjectClientOptions.ProgressBars = false
	}

	log.Debugln("Len of source:", len(args))
	if len(args) < 2 {
		log.Errorln("No Source or Destination")
		err = cmd.Help()
		if err != nil {
			log.Errorln("Failed to print out help:", err)
		}
		os.Exit(1)
	}
	source := args[:len(args)-1]
	dest := args[len(args)-1]

	log.Debugln("Sources:", source)
	log.Debugln("Destination:", dest)

	if len(source) > 1 {
		if destStat, err := os.Stat(dest); err != nil && destStat.IsDir() {
			log.Errorln("Destination is not a directory")
			os.Exit(1)
		}
	}

	var result error
	lastSrc := ""
	for _, src := range source {
		isRecursive, _ := cmd.Flags().GetBool("recursive")
		client.ObjectClientOptions.Recursive = isRecursive
		_, result = client.DoPut(src, dest, isRecursive)
		if result != nil {
			lastSrc = src
			break
		} else {
			client.ClearErrors()
		}
	}

	// Exit with failure
	if result != nil {
		// Print the list of errors
		errMsg := client.GetErrors()
		if errMsg == "" {
			errMsg = result.Error()
		}
		log.Errorln("Failure putting " + lastSrc + ": " + errMsg)
		if client.ErrorsRetryable() {
			log.Errorln("Errors are retryable")
			os.Exit(11)
		}
		os.Exit(1)
	}

}
