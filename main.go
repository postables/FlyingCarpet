package main

import (
	"bufio"
	"context"
	"crypto/md5"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"os/signal"
	"syscall"
	"time"
)

const dialTimeout = 60
const joinAdHocTimeout = 60
const findMacTimeout = 60

// The Transfer struct holds transfer-specific data used throughout program.
// Should reorganize/clean this up but not sure how best to do so.
type Transfer struct {
	Filepath       string
	FileList       []string
	Passphrase     string
	SSID           string
	RecipientIP    string
	Peer           string // "mac", "windows", or "linux"
	Mode           string // "sending" or "receiving"
	PreviousSSID   string
	Port           int
	AdHocCapable   bool
	Ctx            context.Context
	CancelCtx      context.CancelFunc
	WfdSendChan    chan string
	WfdRecvChan    chan string
}

func main() {

	// get flags
	if len(os.Args) == 1 {
		printUsage()
		return
	}

	var pOutFile = flag.String("send", "", "File to be sent. (Use [ -send multi ] for multiple files, and list files/globs after other flags.)\n\n"+
		"Example (Windows): .\\flyingcarpet.exe -send multi -peer mac pic1.jpg pic35.jpg \"filename with spaces.docx\" *.txt\n"+
		"Example (macOS/Linux): ./flyingcarpet -send multi -peer windows movie.mp4 ../*.mp3\n")
	var pInFolder = flag.String("receive", "", "Destination directory for files to be received.")
	var pPort = flag.Int("port", 3290, "TCP port to use (must match on both ends).")
	var pPeer = flag.String("peer", "", "Use \"-peer linux\", \"-peer mac\", or \"-peer windows\" to match the other computer.")
	flag.Parse()
	outFile := *pOutFile
	inFolder := *pInFolder
	port := *pPort
	peer := *pPeer

	// validate
	if peer == "" || (peer != "mac" && peer != "windows" && peer != "linux") {
		log.Fatal("Must choose [ -peer linux|mac|windows ].")
	}

	wfds, wfdr := make(chan string), make(chan string)
	ctx, cancelCtx := context.WithCancel(context.Background())
	var listener *net.TCPListener
	var conn *net.Conn

	t := &Transfer{
		WfdSendChan: wfds,
		WfdRecvChan: wfdr,
		Port:        port,
		Peer:        peer,
		Ctx:         ctx,
		CancelCtx:   cancelCtx,
	}

	// parse flags
	if outFile == "multi" { // -send multi
		t.Mode = "sending"
		baseList := flag.Args()
		var finalList []string
		for _, filename := range baseList {
			expandedList, err := filepath.Glob(filename)
			if err != nil {
				t.output(fmt.Sprintf("Error expanding glob %s: %s", filename, err))
			}
			for _, v := range expandedList {
				v, err = filepath.Abs(v)
				if err != nil {
					t.output(fmt.Sprintf("Error getting abs path for %s: %s", v, err))
				}
				finalList = append(finalList, v)
			}
		}
		if len(finalList) == 0 {
			t.output("No files found to send! When using [ -send multi ], list files to send after other flags. Wildcards accepted.")
			printUsage()
			return
		}
		t.FileList = finalList
		// fmt.Println(t.FileList)
	} else if outFile == "" && inFolder != "" { // receiving
		t.Mode = "receiving"
		path, err := filepath.Abs(inFolder)
		if err != nil {
			t.output(fmt.Sprintf("Error getting abs path for %s: %s", inFolder, err))
		}
		t.Filepath = path + string(os.PathSeparator)
	} else if outFile != "" && inFolder == "" { // sending single file
		t.Mode = "sending"
		t.Filepath = outFile
		t.FileList = append(t.FileList, t.Filepath)
	} else {
		printUsage()
		return
	}

	var err error

	// cleanup
	defer func() {
		shutdownTCP(listener, conn, t)
		resetWifi(t)
	}()

	// trap SIGINT to teardown wifi before exiting in case user hits Ctrl-C
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT)
	go func(t *Transfer) {
		<-sigChan
		t.output("Received interrupt signal, resetting WiFi and exiting.")
		// t.CancelCtx()
		shutdownTCP(listener, conn, t)
		resetWifi(t)
		os.Exit(45)
	}(t)

	// main event
	if t.Mode == "sending" {
		// to stop searching for ad hoc network (if Mac jumps off)
		defer func() {
			if runtime.GOOS == "darwin" {
				t.CancelCtx()
			}
		}()

		t.Passphrase = getPassword()

		pwBytes := md5.Sum([]byte(t.Passphrase))
		prefix := pwBytes[:3]
		t.SSID = fmt.Sprintf("flyingCarpet_%x", prefix)

		if runtime.GOOS == "windows" {
			t.PreviousSSID = getCurrentWifi(t)
		} else if runtime.GOOS == "linux" {
			t.PreviousSSID = getCurrentUUID(t)
		}
		// make ip connection
		if err = connectToPeer(t); err != nil {
			t.output(err.Error())
			t.output("Aborting transfer.")
			return
		}
		// make tcp connection
		conn, err = dialPeer(t)
		if err != nil {
			t.output(err.Error())
			t.output("Could not establish TCP connection with peer. Aborting transfer.")
			return
		}
		t.output("Connected")

		// tell receiving end how many files we're sending
		if err = sendCount(conn, t); err != nil {
			t.output("Could not send number of files: " + err.Error())
			return
		}

		// send files
		for i, v := range t.FileList {
			if len(t.FileList) > 1 {
				t.output("=============================")
				t.output(fmt.Sprintf("Beginning transfer %d of %d. Filename: %s", i+1, len(t.FileList), v))
			}
			t.Filepath = v
			if err = chunkAndSend(conn, t); err != nil {
				t.output(err.Error())
				t.output("Aborting transfer.")
				return
			}
		}

		t.output("Send complete, resetting WiFi and exiting.")

	} else if t.Mode == "receiving" {
		defer func() {
			// why the && here? because if we're on darwin and receiving from darwin, we'll be hosting the adhoc and thus haven't joined it,
			// and thus don't need to shut down the goroutine trying to stay on it. does this need to happen when peer is linux? yes.
			if runtime.GOOS == "darwin" && (t.Peer == "windows" || t.Peer == "linux") {
				t.CancelCtx()
			}
		}()

		t.Passphrase = generatePassword()
		pwBytes := md5.Sum([]byte(t.Passphrase))
		prefix := pwBytes[:3]
		t.SSID = fmt.Sprintf("flyingCarpet_%x", prefix)

		t.output(fmt.Sprintf("=============================\n"+
			"Transfer password: %s\nPlease use this password on sending end when prompted to start transfer.\n"+
			"=============================\n", t.Passphrase))

		// make ip connection
		if err = connectToPeer(t); err != nil {
			t.output(err.Error())
			t.output("Aborting transfer.")
			return
		}

		// make tcp connection
		listener, conn, err = listenForPeer(t)

		if err != nil {
			t.output(err.Error())
			t.output("Aborting transfer.")
			return
		}

		// find out how many files we're receiving
		numFiles, err := receiveCount(conn, t)
		if err != nil {
			t.output("Could not receive number of files: " + err.Error())
			return
		}

		// receive files
		for i := 0; i < numFiles; i++ {
			if numFiles > 1 {
				t.output("=============================")
				t.output(fmt.Sprintf("Receiving file %d of %d.", i+1, numFiles))
			}
			if err = receiveAndAssemble(conn, t); err != nil {
				t.output(err.Error())
				t.output("Aborting transfer.")
				return
			}
		}

		t.output("Reception complete, resetting WiFi and exiting.")
	}
}

func listenForPeer(t *Transfer) (*net.TCPListener, *net.Conn, error) {
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{Port: t.Port})
	if err != nil {
		return nil, nil, fmt.Errorf("Could not listen on :%d. Err: %s", t.Port, err)
	}
	t.output("Listening on :" + strconv.Itoa(t.Port))

	for {
		select {
		case <-t.Ctx.Done():
			return nil, nil, errors.New("Exiting listenForPeer, transfer was canceled.")
		default:
			ln.SetDeadline(time.Now().Add(time.Second))
			conn, err := ln.Accept()
			if err != nil {
				// t.output("Error accepting connection: " + err.Error())
				continue
			}
			t.output("Connection accepted")
			return ln, &conn, nil
		}
	}
}

func dialPeer(t *Transfer) (*net.Conn, error) {
	var conn net.Conn
	var err error
	t.output("Trying to connect to " + t.RecipientIP + " for " + strconv.Itoa(dialTimeout) + " seconds.")
	for i := 0; i < dialTimeout; i++ {
		select {
		case <-t.Ctx.Done():
			return nil, errors.New("Exiting dialPeer, transfer was canceled.")
		default:
			err = nil
			conn, err = net.DialTimeout("tcp", t.RecipientIP+":"+strconv.Itoa(t.Port), time.Millisecond*10)
			if err != nil {
				// t.output(fmt.Sprintf("Failed connection %2d to %s, retrying.", i, t.RecipientIP))
				time.Sleep(time.Second * 1)
				continue
			}
			t.output("Successfully dialed peer.")
			return &conn, nil
		}
	}
	return nil, fmt.Errorf("Waited %d seconds, no connection.", dialTimeout)
}

func generatePassword() string {
	// no l, I, or O because they look too similar to each other, 1, and 0
	const chars = "0123456789abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ"
	rand.Seed(time.Now().UTC().UnixNano())
	pwBytes := make([]byte, 4)
	for i := range pwBytes {
		pwBytes[i] = chars[rand.Intn(len(chars))]
	}
	return string(pwBytes)
}

func getPassword() (pw string) {
	reader := bufio.NewReader(os.Stdin)
	fmt.Print("Enter password from receiving end: ")
	pw, err := reader.ReadString('\n')
	if err != nil {
		fmt.Println("Error getting password:", err)
	}
	pw = strings.TrimSpace(pw)
	return
}

func printUsage() {
	fmt.Println("\nSingle file usage:")
	fmt.Println("(Windows) $ flyingcarpet.exe -send ./movie.mp4 -peer mac")
	fmt.Println("[Enter password from receiving end.]")
	fmt.Println("  (Mac)   $ ./flyingcarpet -receive ./destinationFolder -peer windows")
	fmt.Println("[Enter password into sending end.]\n")

	fmt.Println("Multiple file usage:")
	fmt.Println(" (Linux)  $ ./flyingcarpet -send multi -peer windows ../Pictures/*.jpg \"Filename with spaces.txt\" movie.mp4")
	fmt.Println("[Enter password from receiving end.]")
	fmt.Println("(Windows) $ flyingcarpet.exe -receive .\\picturesFolder -peer linux")
	fmt.Println("[Enter password into sending end.]\n")
	return
}

func (t *Transfer) output(msg string) {
	fmt.Println(msg)
}

func shutdownTCP(listener *net.TCPListener, conn *net.Conn, t *Transfer) {
	if conn != nil {
		if err := (*conn).Close(); err != nil {
			t.output("Error closing TCP connection: " + err.Error())
		} else {
			t.output("Closed TCP connection")
		}
	} else {
		t.output("conn was nil")
	}
	if listener != nil {
		if err := (*listener).Close(); err != nil {
			t.output("Error closing TCP listener: " + err.Error())
		} else {
			t.output("Closed TCP listener")
		}
	} else {
		t.output("listener was nil")
	}
}