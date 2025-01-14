/*
MIT License

Copyright (c) 2022 Lonny Wong

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package trzsz

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/ncruces/zenity"
	"golang.org/x/term"
)

type TrzszArgs struct {
	Help     bool
	Version  bool
	TraceLog bool
	DragFile bool
	Name     string
	Args     []string
}

var gTrzszArgs *TrzszArgs
var gTraceLog *os.File = nil
var gDragging int32 = 0
var gDragHasDir int32 = 0
var gDragMutex sync.Mutex
var gDragFiles []string = nil
var gInterrupting int32 = 0
var gSkipTrzCommand int32 = 0
var gTransfer *TrzszTransfer = nil
var gUniqueIDMap = make(map[string]int)
var parentWindowID = getParentWindowID()
var trzszRegexp = regexp.MustCompile("::TRZSZ:TRANSFER:([SRD]):(\\d+\\.\\d+\\.\\d+)(:\\d+)?")

func printVersion() {
	fmt.Printf("trzsz go %s\n", kTrzszVersion)
}

func printHelp() {
	fmt.Print("usage: trzsz [-h] [-v] [-t] [-d] command line\n\n" +
		"Wrapping command line to support trzsz ( trz / tsz ).\n\n" +
		"positional arguments:\n" +
		"  command line       the original command line\n\n" +
		"optional arguments:\n" +
		"  -h, --help         show this help message and exit\n" +
		"  -v, --version      show version number and exit\n" +
		"  -t, --tracelog     eanble trace log for debugging\n" +
		"  -d, --dragfile     enable drag file(s) to upload\n")
}

func parseTrzszArgs() {
	gTrzszArgs = &TrzszArgs{false, false, false, false, "", nil}
	var i int
	for i = 1; i < len(os.Args); i++ {
		if os.Args[i] == "-h" || os.Args[i] == "--help" {
			gTrzszArgs.Help = true
			return
		} else if os.Args[i] == "-v" || os.Args[i] == "--version" {
			gTrzszArgs.Version = true
			return
		} else if os.Args[i] == "-t" || os.Args[i] == "--tracelog" {
			gTrzszArgs.TraceLog = true
		} else if os.Args[i] == "-d" || os.Args[i] == "--dragfile" {
			gTrzszArgs.DragFile = true
		} else {
			break
		}
	}
	if i >= len(os.Args) {
		gTrzszArgs.Help = true
		return
	}
	gTrzszArgs.Name = os.Args[i]
	gTrzszArgs.Args = os.Args[i+1:]
}

func getTrzszConfig(name string) *string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	file, err := os.Open(filepath.Join(home, ".trzsz.conf"))
	if err != nil {
		return nil
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		idx := strings.Index(line, "=")
		if idx < 0 {
			continue
		}
		if strings.TrimSpace(line[0:idx]) == name {
			value := strings.TrimSpace(line[idx+1:])
			if len(value) == 0 {
				return nil
			}
			return &value
		}
	}
	return nil
}

func detectTrzsz(output []byte) (*byte, bool) {
	if len(output) < 24 {
		return nil, false
	}
	idx := bytes.LastIndex(output, []byte("::TRZSZ:TRANSFER:"))
	if idx < 0 {
		return nil, false
	}
	match := trzszRegexp.FindSubmatch(output[idx:])
	if len(match) < 2 {
		return nil, false
	}
	uniqueID := ""
	if len(match) > 3 {
		uniqueID = string(match[3])
	}
	if len(uniqueID) >= 8 {
		if _, ok := gUniqueIDMap[uniqueID]; ok {
			return nil, false
		}
		if len(gUniqueIDMap) > 100 {
			m := make(map[string]int)
			for k, v := range gUniqueIDMap {
				if v >= 50 {
					m[k] = v - 50
				}
			}
			gUniqueIDMap = m
		}
		gUniqueIDMap[uniqueID] = len(gUniqueIDMap)
	}
	remoteIsWindows := uniqueID == ":1"
	return &match[1][0], remoteIsWindows
}

func chooseDownloadPath() (string, error) {
	savePath := getTrzszConfig("DefaultDownloadPath")
	if savePath != nil {
		return *savePath, nil
	}
	path, err := zenity.SelectFile(
		zenity.Title("Choose a folder to save file(s)"),
		zenity.Directory(),
		zenity.ShowHidden(),
		zenity.Attach(parentWindowID),
	)
	if err != nil {
		return "", err
	}
	if len(path) == 0 {
		return "", zenity.ErrCanceled
	}
	return path, nil
}

func chooseUploadPaths(directory bool) ([]string, error) {
	if atomic.LoadInt32(&gDragging) != 0 {
		files := resetDragFiles()
		return files, nil
	}
	options := []zenity.Option{
		zenity.Title("Choose some files to send"),
		zenity.ShowHidden(),
		zenity.Attach(parentWindowID),
	}
	defaultPath := getTrzszConfig("DefaultUploadPath")
	if defaultPath != nil {
		options = append(options, zenity.Filename(*defaultPath))
	}
	if directory {
		options = append(options, zenity.Directory())
	}
	files, err := zenity.SelectFileMutiple(options...)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, zenity.ErrCanceled
	}
	return files, nil
}

func newProgressBar(pty *TrzszPty, config map[string]interface{}) (*TextProgressBar, error) {
	quiet := false
	if v, ok := config["quiet"].(bool); ok {
		quiet = v
	}
	if quiet {
		return nil, nil
	}
	columns, err := pty.GetColumns()
	if err != nil {
		return nil, err
	}
	tmuxPaneColumns := -1
	if v, ok := config["tmux_pane_width"].(float64); ok {
		tmuxPaneColumns = int(v)
	}
	return NewTextProgressBar(os.Stdout, columns, tmuxPaneColumns), nil
}

func downloadFiles(pty *TrzszPty, transfer *TrzszTransfer, remoteIsWindows bool) error {
	path, err := chooseDownloadPath()
	if err == zenity.ErrCanceled {
		return transfer.sendAction(false, remoteIsWindows)
	}
	if err != nil {
		return err
	}
	if err := checkPathWritable(path); err != nil {
		return err
	}

	if err := transfer.sendAction(true, remoteIsWindows); err != nil {
		return err
	}
	config, err := transfer.recvConfig()
	if err != nil {
		return err
	}

	progress, err := newProgressBar(pty, config)
	if err != nil {
		return err
	}
	if progress != nil && !reflect.ValueOf(progress).IsNil() {
		pty.OnResize(func(cols int) { progress.setTerminalColumns(cols) })
		defer pty.OnResize(nil)
	}

	localNames, err := transfer.recvFiles(path, progress)
	if err != nil {
		return err
	}

	return transfer.clientExit(fmt.Sprintf("Saved %s to %s", strings.Join(localNames, ", "), path))
}

func uploadFiles(pty *TrzszPty, transfer *TrzszTransfer, directory, remoteIsWindows bool) error {
	paths, err := chooseUploadPaths(directory)
	if err == zenity.ErrCanceled {
		return transfer.sendAction(false, remoteIsWindows)
	}
	if err != nil {
		return err
	}
	files, err := checkPathsReadable(paths, directory)
	if err != nil {
		return err
	}

	if err := transfer.sendAction(true, remoteIsWindows); err != nil {
		return err
	}
	config, err := transfer.recvConfig()
	if err != nil {
		return err
	}

	overwrite := false
	if v, ok := config["overwrite"].(bool); ok {
		overwrite = v
	}
	if overwrite {
		if err := checkDuplicateNames(files); err != nil {
			return err
		}
	}

	progress, err := newProgressBar(pty, config)
	if err != nil {
		return err
	}
	if progress != nil && !reflect.ValueOf(progress).IsNil() {
		pty.OnResize(func(cols int) { progress.setTerminalColumns(cols) })
		defer pty.OnResize(nil)
	}

	remoteNames, err := transfer.sendFiles(files, progress)
	if err != nil {
		return err
	}

	return transfer.clientExit(fmt.Sprintf("Received %s", strings.Join(remoteNames, ", ")))
}

func handleTrzsz(pty *TrzszPty, mode byte, remoteIsWindows bool) {
	transfer := NewTransfer(pty.Stdin, nil)

	atomic.StorePointer((*unsafe.Pointer)(unsafe.Pointer(&gTransfer)), unsafe.Pointer(transfer))
	defer func() {
		atomic.StorePointer((*unsafe.Pointer)(unsafe.Pointer(&gTransfer)), unsafe.Pointer(nil))
	}()

	defer func() {
		if err := recover(); err != nil {
			transfer.clientError(NewTrzszError(fmt.Sprintf("%v", err), "panic", true))
		}
	}()

	var err error
	switch mode {
	case 'S':
		err = downloadFiles(pty, transfer, remoteIsWindows)
	case 'R':
		err = uploadFiles(pty, transfer, false, remoteIsWindows)
	case 'D':
		err = uploadFiles(pty, transfer, true, remoteIsWindows)
	}
	if err != nil {
		transfer.clientError(err)
	}
}

func resetDragFiles() []string {
	if atomic.LoadInt32(&gDragging) == 0 {
		return nil
	}
	gDragMutex.Lock()
	defer gDragMutex.Unlock()
	atomic.StoreInt32(&gDragging, 0)
	atomic.StoreInt32(&gDragHasDir, 0)
	dragFiles := gDragFiles
	gDragFiles = nil
	return dragFiles
}

func addDragFiles(dragFiles []string, hasDir bool) bool {
	gDragMutex.Lock()
	defer gDragMutex.Unlock()
	atomic.StoreInt32(&gDragging, 1)
	if hasDir {
		atomic.StoreInt32(&gDragHasDir, 1)
	}
	if gDragFiles == nil {
		gDragFiles = dragFiles
		return true
	}
	gDragFiles = append(gDragFiles, dragFiles...)
	return false
}

func uploadDragFiles(pty *TrzszPty) {
	time.Sleep(300 * time.Millisecond)
	if atomic.LoadInt32(&gDragging) == 0 {
		return
	}
	atomic.StoreInt32(&gInterrupting, 1)
	pty.Stdin.Write([]byte{0x03})
	time.Sleep(200 * time.Millisecond)
	atomic.StoreInt32(&gInterrupting, 0)
	atomic.StoreInt32(&gSkipTrzCommand, 1)
	if atomic.LoadInt32(&gDragHasDir) != 0 {
		pty.Stdin.Write([]byte("trz -d\r"))
	} else {
		pty.Stdin.Write([]byte("trz\r"))
	}
	time.Sleep(time.Second)
	resetDragFiles()
}

func writeTraceLog(buf []byte, output bool) []byte {
	// Windows disable log: echo ^<DISABLE_TRZSZ_TRACE_LOG^>
	// Linux macOS disable log: echo -e '\x3CDISABLE_TRZSZ_TRACE_LOG\x3E'
	if gTraceLog != nil {
		if output && bytes.Contains(buf, []byte("<DISABLE_TRZSZ_TRACE_LOG>")) {
			msg := fmt.Sprintf("Closed trace log at %s", gTraceLog.Name())
			gTraceLog.Close()
			gTraceLog = nil
			return bytes.ReplaceAll(buf, []byte("<DISABLE_TRZSZ_TRACE_LOG>"), []byte(msg))
		}
		typ := "in"
		if output {
			typ = "out"
		}
		gTraceLog.WriteString(fmt.Sprintf("[%s]%s\n", typ, encodeBytes(buf)))
		gTraceLog.Sync()
		return buf
	}
	// Windows enable log: echo ^<ENABLE_TRZSZ_TRACE_LOG^>
	// Linux macOS enable log: echo -e '\x3CENABLE_TRZSZ_TRACE_LOG\x3E'
	if output && bytes.Contains(buf, []byte("<ENABLE_TRZSZ_TRACE_LOG>")) {
		var err error
		var msg string
		gTraceLog, err = os.CreateTemp("", "trzsz_*.log")
		if err != nil {
			msg = fmt.Sprintf("Create log file error: %v", err)
		} else {
			msg = fmt.Sprintf("Writing trace log to %s", gTraceLog.Name())
		}
		return bytes.ReplaceAll(buf, []byte("<ENABLE_TRZSZ_TRACE_LOG>"), []byte(msg))
	}
	return buf
}

func wrapInput(pty *TrzszPty) {
	buffer := make([]byte, 10240)
	for {
		n, err := os.Stdin.Read(buffer)
		if err == io.EOF {
			if IsWindows() { // ctrl + z
				n = 1
				err = nil
				buffer[0] = 0x1A
			} else {
				pty.Stdin.Close()
				break
			}
		}
		if err == nil && n > 0 {
			buf := buffer[0:n]
			if gTrzszArgs.TraceLog {
				buf = writeTraceLog(buf, false)
			}
			if transfer := (*TrzszTransfer)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&gTransfer)))); transfer != nil {
				if buf[0] == '\x03' { // `ctrl + c` to stop transferring files
					transfer.stopTransferringFiles()
				}
				continue
			}
			if gTrzszArgs.DragFile {
				dragFiles, hasDir, ignore := detectDragFiles(buf)
				if dragFiles != nil {
					if addDragFiles(dragFiles, hasDir) {
						go uploadDragFiles(pty)
					}
					continue
				}
				if !ignore {
					resetDragFiles()
				}
			}
			pty.Stdin.Write(buf)
		}
	}
}

func wrapOutput(pty *TrzszPty) {
	const bufSize = 10240
	buffer := make([]byte, bufSize)
	for {
		n, err := pty.Stdout.Read(buffer)
		if err == io.EOF {
			os.Stdout.Close()
			break
		} else if err == nil && n > 0 {
			buf := buffer[0:n]
			if gTrzszArgs.TraceLog {
				buf = writeTraceLog(buf, true)
			}
			if transfer := (*TrzszTransfer)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&gTransfer)))); transfer != nil {
				transfer.addReceivedData(buf)
				buffer = make([]byte, bufSize)
				continue
			}
			mode, remoteIsWindows := detectTrzsz(buf)
			if mode != nil {
				os.Stdout.Write(bytes.Replace(buf, []byte("TRZSZ"), []byte("TRZSZGO"), 1))
				go handleTrzsz(pty, *mode, remoteIsWindows)
				continue
			}
			if atomic.LoadInt32(&gInterrupting) != 0 {
				continue
			}
			if atomic.LoadInt32(&gSkipTrzCommand) != 0 {
				atomic.StoreInt32(&gSkipTrzCommand, 0)
				output := strings.TrimRight(string(trimVT100(buf)), "\r\n")
				if output == "trz" || output == "trz -d" {
					os.Stdout.WriteString("\r\n")
					continue
				}
			}
			os.Stdout.Write(buf)
		}
	}
}

func handleSignal(pty *TrzszPty) {
	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGTERM)
	go func() {
		<-sigterm
		pty.Terminate()
	}()

	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, os.Interrupt)
	go func() {
		for {
			<-sigint
			if transfer := (*TrzszTransfer)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&gTransfer)))); transfer != nil {
				transfer.stopTransferringFiles()
			}
		}
	}()
}

// TrzszMain entry of trzsz client
func TrzszMain() int {
	// parse command line arguments
	parseTrzszArgs()
	if gTrzszArgs.Help {
		printHelp()
		return 0
	}
	if gTrzszArgs.Version {
		printVersion()
		return 0
	}

	// spawn a pty
	pty, err := Spawn(gTrzszArgs.Name, gTrzszArgs.Args...)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return -1
	}
	defer func() { pty.Close() }()

	// set stdin in raw mode
	if state, err := term.MakeRaw(int(os.Stdin.Fd())); err == nil {
		defer func() { _ = term.Restore(int(os.Stdin.Fd()), state) }()
	}

	// wrap input and output
	go wrapInput(pty)
	go wrapOutput(pty)

	// handle signal
	go handleSignal(pty)

	// wait for exit
	pty.Wait()
	return pty.ExitCode()
}
