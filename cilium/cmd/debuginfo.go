// Copyright 2017 Authors of Cilium
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

package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cilium/cilium/api/v1/models"
	pkg "github.com/cilium/cilium/pkg/client"
	"github.com/cilium/cilium/pkg/command"

	"github.com/russross/blackfriday"
	"github.com/spf13/cobra"
)

// outputTypes enum definition
type outputType int

// outputTypes enum values
const (
	STDOUT outputType = 0 + iota
	MARKDOWN
	HTML
	JSONOUTPUT
	JSONPATH
)

var (
	// Can't tall it jsonOutput because another var in this package uses that.
	jsonOutputDebuginfo = "JSON"
	markdownOutput      = "MARKDOWN"
	htmlOutput          = "HTML"
	jsonpathOutput      = "JSONPATH"
	re                  = regexp.MustCompile(`^jsonpath\=(.*)`)
)

// outputTypes enum strings
var outputTypes = [...]string{
	"STDOUT",
	markdownOutput,
	htmlOutput,
	jsonOutputDebuginfo,
	jsonpathOutput,
}

var debuginfoCmd = &cobra.Command{
	Use:   "debuginfo",
	Short: "Request available debugging information from agent",
	Run:   runDebugInfo,
}

var (
	outputToFile   bool
	html           string
	filePerCommand bool
	outputOpts     []string
	outputDir      string
)

type addSection func(*tabwriter.Writer, *models.DebugInfo)

var sections = map[string]addSection{
	"cilium-version":          addCiliumVersion,
	"kernel-version":          addKernelVersion,
	"cilium-status":           addCiliumStatus,
	"cilium-environment-keys": addCiliumEnvironmentKeys,
	"cilium-endpoint-list":    addCiliumEndpointList,
	"cilium-service-list":     addCiliumServiceList,
	"cilium-policy":           addCiliumPolicy,
	"cilium-memory-map":       addCiliumMemoryMap,
}

func init() {
	rootCmd.AddCommand(debuginfoCmd)
	debuginfoCmd.Flags().BoolVarP(&outputToFile, "file", "f", false, "Redirect output to file(s)")
	debuginfoCmd.Flags().BoolVarP(&filePerCommand, "file-per-command", "", false, "Generate a single file per command")
	debuginfoCmd.Flags().StringSliceVar(&outputOpts, "output", []string{}, "markdown| html| json| jsonpath='{}'")
	debuginfoCmd.Flags().StringVar(&outputDir, "output-directory", "", "directory for files (if specified will use directory in which this command was ran)")
}

func validateInput() {
	if outputDir != "" && !outputToFile {
		fmt.Fprintf(os.Stderr, "invalid option combination; specified output-directory %q, but did not specify for output to be redirected to file; exiting\n", outputDir)
		os.Exit(1)
	}
	validateOutputOpts()
}

func validateOutputOpts() {

	for _, outputOpt := range outputOpts {
		switch strings.ToUpper(outputOpt) {
		case markdownOutput:
		case htmlOutput:
			if !outputToFile {
				fmt.Fprintf(os.Stderr, "if HTML is specified as the output format, it is required that you provide the `--file` argument as well\n")
				os.Exit(1)
			}
		case jsonOutputDebuginfo:
			if filePerCommand {
				fmt.Fprintf(os.Stderr, "%s does not support dumping a file per command; exiting\n", jsonOutputDebuginfo)
				os.Exit(1)
			}
		case jsonpathOutput:
			if filePerCommand {
				fmt.Fprintf(os.Stderr, "%s does not support dumping a file per command; exiting\n", jsonpathOutput)
				os.Exit(1)
			}
		default:
			// Check to see if arg contains jsonpath filtering as well.
			if re.MatchString(outputOpt) {
				return
			}
			fmt.Fprintf(os.Stderr, "%s is not a valid output format; exiting\n", outputOpt)
			os.Exit(1)
		}
	}
}

func formatFileName(outputDir string, cmdTime time.Time, outtype outputType) string {
	var fileName string
	var sep string
	if outputDir != "" {
		sep = outputDir + "/"
	}
	timeStr := cmdTime.Format("20060102-150405.999-0700-MST")
	switch outtype {
	case MARKDOWN:
		fileName = fmt.Sprintf("%scilium-debuginfo-%s.md", sep, timeStr)
	case HTML:
		fileName = fmt.Sprintf("%scilium-debuginfo-%s.html", sep, timeStr)
	case JSONOUTPUT:
		fileName = fmt.Sprintf("%scilium-debuginfo-%s.json", sep, timeStr)
	case JSONPATH:
		fileName = fmt.Sprintf("%scilium-debuginfo-%s.jsonpath", sep, timeStr)
	default:
		fileName = fmt.Sprintf("%scilium-debuginfo-%s.md", sep, timeStr)
	}
	return fileName
}

func runDebugInfo(cmd *cobra.Command, args []string) {

	validateInput()

	// Only warn when not dumping output as JSON so that when the output of the
	// command is specified to be JSON, the only outputted content is the JSON
	// model of debuginfo.
	if os.Getuid() != 0 && !command.OutputJSON() {
		fmt.Fprint(os.Stderr, "Warning, some of the BPF commands might fail when not run as root\n")
	}

	resp, err := client.Daemon.GetDebuginfo(nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", pkg.Hint(err))
		os.Exit(1)
	}

	var output outputType

	// create tab-writer to fill buffer
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 5, 0, 3, ' ', 0)
	p := resp.Payload

	cmdTime := time.Now()

	if outputToFile && len(outputOpts) == 0 {
		outputOpts = append(outputOpts, markdownOutput)
	}

	// Dump payload for each output format.
	for _, outputOpt := range outputOpts {
		var fileName string

		switch strings.ToUpper(outputOpt) {
		case markdownOutput:
			output = MARKDOWN
		case htmlOutput:
			output = HTML
		case jsonOutputDebuginfo:
			output = JSONOUTPUT
		case jsonpathOutput:
			output = JSONPATH
		default:
			if re.MatchString(outputOpt) {
				output = JSONPATH
			}
		}

		if outputToFile {
			fileName = formatFileName(outputDir, cmdTime, output)
		}

		// Generate multiple files for each subsection of the command if
		// specified, except in the JSON cases.
		if filePerCommand && (output != JSONOUTPUT && output != JSONPATH) {
			for cmdName, section := range sections {
				addHeader(w)
				section(w, p)
				writeToOutput(buf, output, fileName, cmdName)
				buf.Reset()
			}
			continue
		}

		// Generate a single file, except not for JSON; no formatting is
		// needed.
		if output != JSONOUTPUT && output != JSONPATH {
			addHeader(w)
			for _, section := range sections {
				section(w, p)
			}
			writeToOutput(buf, output, fileName, "")
			buf.Reset()
		} else {
			marshaledDebugInfo, _ := p.MarshalBinary()
			buf.Write(marshaledDebugInfo)
			if output == JSONOUTPUT {
				writeToOutput(buf, output, fileName, "")
			} else {
				writeJSONPathToOutput(buf, fileName, "", outputOpt)
			}
			buf.Reset()
		}
	}

	if len(outputOpts) > 0 {
		return
	}

	// Just write to stdout in markdown formats if no output option specified.
	addHeader(w)
	for _, section := range sections {
		section(w, p)
	}
	writeToOutput(buf, STDOUT, "", "")

}

func addHeader(w *tabwriter.Writer) {
	fmt.Fprintf(w, "# Cilium debug information\n")
}

func addCiliumVersion(w *tabwriter.Writer, p *models.DebugInfo) {
	printMD(w, "Cilium version", p.CiliumVersion)
}

func addKernelVersion(w *tabwriter.Writer, p *models.DebugInfo) {
	printMD(w, "Kernel version", p.KernelVersion)
}

func addCiliumStatus(w *tabwriter.Writer, p *models.DebugInfo) {
	printMD(w, "Cilium status", "")
	printTicks(w)
	pkg.FormatStatusResponse(w, p.CiliumStatus, true, true, true, true)
	printTicks(w)
}

func addCiliumEnvironmentKeys(w *tabwriter.Writer, p *models.DebugInfo) {
	printMD(w, "Cilium environment keys", strings.Join(p.EnvironmentVariables, "\n"))
}

func addCiliumEndpointList(w *tabwriter.Writer, p *models.DebugInfo) {
	printMD(w, "Endpoint list", "")
	printTicks(w)
	printEndpointList(w, p.EndpointList)
	printTicks(w)

	for _, ep := range p.EndpointList {
		epID := strconv.FormatInt(ep.ID, 10)
		printList(w, "BPF Policy Get "+epID, "bpf", "policy", "get", epID)
		printList(w, "BPF CT List "+epID, "bpf", "ct", "list", epID)
		printList(w, "Endpoint Get "+epID, "endpoint", "get", epID)
		printList(w, "Endpoint Health "+epID, "endpoint", "health", epID)
		printList(w, "Endpoint Log "+epID, "endpoint", "log", epID)

		if ep.Status != nil && ep.Status.Identity != nil {
			id := strconv.FormatInt(ep.Status.Identity.ID, 10)
			printList(w, "Identity get "+id, "identity", "get", id)
		}
	}
}

func addCiliumServiceList(w *tabwriter.Writer, p *models.DebugInfo) {
	printMD(w, "Service list", "")
	printTicks(w)
	printServiceList(w, p.ServiceList)
	printTicks(w)
}

func addCiliumPolicy(w *tabwriter.Writer, p *models.DebugInfo) {
	printMD(w, "Policy get", fmt.Sprintf(":\n %s\nRevision: %d\n", p.Policy.Policy, p.Policy.Revision))
}

func addCiliumMemoryMap(w *tabwriter.Writer, p *models.DebugInfo) {
	printMD(w, "Cilium memory map\n", p.CiliumMemoryMap)
	if nm := p.CiliumNodemonitorMemoryMap; len(nm) > 0 {
		printMD(w, "Cilium nodemonitor memory map", p.CiliumNodemonitorMemoryMap)
	}
}

func writeJSONPathToOutput(buf bytes.Buffer, path string, suffix string, jsonPath string) {
	data := buf.Bytes()
	db := &models.DebugInfo{}
	err := db.UnmarshalBinary(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error unmarshaling binary: %s\n", err)
	}

	jsonBytes, err := command.DumpJSONToSlice(db, jsonPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error printing JSON: %s\n", err)
	}

	// Print to stdout
	if path == "" {
		fmt.Println(string(jsonBytes[:]))
		return
	}

	fileName := fileName(path, suffix)
	writeJSON(jsonBytes, fileName)
	return

}

func writeToOutput(buf bytes.Buffer, output outputType, path string, suffix string) {
	data := buf.Bytes()

	if path == "" {
		switch output {
		case JSONOUTPUT:
			db := &models.DebugInfo{}
			err := db.UnmarshalBinary(data)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error unmarshaling binary: %s\n", err)
			}

			err = command.PrintOutputWithType(db, "json")
			if err != nil {
				fmt.Fprintf(os.Stderr, "error printing JSON: %s\n", err)
			}
		default:
			fmt.Println(string(data))
		}
		return
	}

	if output == STDOUT {
		// Write to standard output
		fmt.Println(string(data))
		return
	}

	fileName := fileName(path, suffix)

	switch output {
	case MARKDOWN:
		// Markdown file
		writeMarkdown(data, fileName)
	case HTML:
		// HTML file
		writeHTML(data, fileName)
	case JSONOUTPUT:
		writeJSON(data, fileName)
	case JSONPATH:
		writeJSON(data, fileName)
	}

	fmt.Printf("%s output at %s\n", outputTypes[output], fileName)
}

func fileName(path, suffix string) string {
	if len(suffix) == 0 {
		// no suffix, return path
		return path
	}

	ext := filepath.Ext(path)
	if ext != "" {
		// insert suffix and move extension to back
		return fmt.Sprintf("%s-%s%s", strings.TrimSuffix(path, ext), suffix, ext)
	}
	// no extension, just append suffix
	return fmt.Sprintf("%s-%s", path, suffix)
}

func printList(w io.Writer, header string, args ...string) {
	output, _ := exec.Command("cilium", args...).CombinedOutput()
	printMD(w, header, string(output))
}

func printMD(w io.Writer, header string, body string) {
	if len(body) > 0 {
		fmt.Fprintf(w, "\n#### %s\n\n```\n%s\n```\n\n", header, body)
	} else {
		fmt.Fprintf(w, "\n#### %s\n\n", header)
	}
}

func printTicks(w io.Writer) {
	fmt.Fprint(w, "```\n")
}

func writeHTML(data []byte, path string) {
	output := blackfriday.MarkdownCommon(data)
	if err := ioutil.WriteFile(path, output, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error while writing HTML file %s", err)
		return
	}
}

func writeMarkdown(data []byte, path string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not create file %s", path)
	}
	w := tabwriter.NewWriter(f, 5, 0, 3, ' ', 0)
	w.Write(data)
}

func writeJSON(data []byte, path string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not create file %s", path)
		os.Exit(1)
	}

	db := &models.DebugInfo{}

	err = db.UnmarshalBinary(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error unmarshaling binary: %s\n", err)
		os.Exit(1)
	}
	result, err := json.MarshalIndent(db, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error marshal-indenting data: %s\n", err)
		os.Exit(1)
	}
	f.Write(result)

}
