package main

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Gonzih/log-replay/pkg/reader"
	"github.com/Gonzih/log-replay/pkg/reader/haproxy"
	"github.com/Gonzih/log-replay/pkg/reader/nginx"
	"github.com/Gonzih/log-replay/pkg/reader/solr"
	"github.com/mxmCherry/movavg"
)

var windowChannel chan int8
var logChannel chan string
var logWg sync.WaitGroup
var httpWg sync.WaitGroup

var ma *movavg.SMA

var format string
var inputLogFile string
var logFile string
var prefix string
var inputFileType string
var ratio int64
var debug bool
var clientTimeout int64
var skipSleep bool
var enableWindow bool
var windowSize int
var errorRate float64
var sslSkipVerify bool
var basicAuthUser string
var basicAuthPassword string

func init() {
	flag.StringVar(&format, "format", `$remote_addr [$time_local] "$request" $status $request_length $body_bytes_sent $request_time "$t_size" $read_time $gen_time`, "Nginx log format")
	flag.StringVar(&inputLogFile, "file", "-", "Log file name to read. Read from STDIN if file name is '-'")
	flag.StringVar(&logFile, "log", "-", "File to report timings to, default is stdout")
	flag.StringVar(&prefix, "prefix", "http://localhost", "URL prefix to query")
	flag.StringVar(&inputFileType, "file-type", "nginx", "Input log type (nginx, haproxy or solr)")
	flag.Int64Var(&ratio, "ratio", 1, "Replay speed ratio, higher means faster replay speed")
	flag.BoolVar(&debug, "debug", false, "Print extra debugging information")
	flag.Int64Var(&clientTimeout, "timeout", 60000, "Request timeout in milliseconds, 0 means no timeout")
	flag.BoolVar(&skipSleep, "skip-sleep", false, "Skip sleep between http calls based on log timestamps")
	flag.BoolVar(&enableWindow, "enable-window", false, "Enable rolling window functionality to stop log replaying in case of failure")
	flag.IntVar(&windowSize, "window-size", 1000, "Size of the window to track response status")
	flag.Float64Var(&errorRate, "error-rate", 40, "Percentage of the error to stop log replaying (min:1, max:99)")
	flag.BoolVar(&sslSkipVerify, "ssl-skip-verify", false, "Should HTTP client ignore ssl errors")
	flag.StringVar(&basicAuthUser, "user-name", "", "Basic auth username")
	flag.StringVar(&basicAuthPassword, "password", "", "Basic auth password")

	logChannel = make(chan string)
}

func mainLoop(rdr reader.LogReader, transport *http.Transport) {
	var nilTime time.Time
	var lastTime time.Time

	client := &http.Client{
		Transport: transport,
		Timeout:   time.Duration(clientTimeout) * time.Millisecond,
	}

	for {
		rec, err := rdr.Read()

		if err == io.EOF {
			log.Println("Reached EOF")
			break
		} else {
			reader.Must(err)
		}

		if !skipSleep {
			if lastTime != nilTime {

				differenceUnix := rec.Time.Sub(lastTime).Nanoseconds()

				if differenceUnix > 0 {
					durationWithRation := time.Duration(differenceUnix / ratio)

					if debug {
						log.Printf("Sleeping for: %.2f seconds", durationWithRation.Seconds())
					}
					time.Sleep(durationWithRation)
				} else {
					if debug {
						log.Println("No need for sleep!")
					}
				}
			}

			lastTime = rec.Time
		}

		httpWg.Add(1)
		go fireHTTPRequest(client, rec.Method, rec.URL, rec.Payload, rec.UA)
	}
}

func fireHTTPRequest(client *http.Client, method string, url string, payload string, ua string) {
	defer httpWg.Done()

	path := prefix + url

	if debug {
		log.Printf("Querying %s %s %s\n", method, path, payload, ua)
	}

	var logMessage string
	var windowStatus int8

	startTime := time.Now()
	startTS := startTime.Unix()

	req, err := http.NewRequest(method, path, bytes.NewBufferString(payload))

	if method == "POST" {
		req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	}

	if len(basicAuthUser) > 0 && len(basicAuthPassword) > 0 {
		req.SetBasicAuth(basicAuthUser, basicAuthPassword)
	}

	if err != nil {
		if debug {
			log.Printf("ERROR %s while creating new request to %s", err, path)
		}
		logMessage = fmt.Sprintf("%d\t%d\t%d\t%s\t%s\t%s\n", 500, startTS, 0, url, payload, err)
		logChannel <- logMessage

		return
	}

	req.Header.Set("User-Agent", ua)

	resp, err := client.Do(req)

	if (err == nil) {
		_, err = io.Copy(ioutil.Discard, resp.Body)
		resp.Body.Close()
	}

	duration := time.Since(startTime).Nanoseconds()

	if err != nil {
		if debug {
			log.Printf(`ERROR "%s" while querying "%s"`, err, path)
		}
		windowStatus = 1
		logMessage = fmt.Sprintf("%d\t%d\t%d\t%s\t%s\t%s\n", 500, startTS, duration, url, payload, err)
	} else {
		windowStatus = 0
		status := resp.StatusCode
		logMessage = fmt.Sprintf("%d\t%d\t%d\t%s\t%s\n", status, startTS, duration, url, payload)
	}


	if enableWindow {
		windowChannel <- windowStatus
	}
	logChannel <- logMessage
}

func logLoop() {
	defer logWg.Done()

	var writer io.Writer

	switch logFile {
	case "-":
		writer = os.Stdout
	default:
		file, err := os.Create(logFile)
		reader.Must(err)
		defer file.Close()
		writer = file
	}

	for logMessage := range logChannel {
		_, err := io.WriteString(writer, logMessage)
		reader.Must(err)
	}
}

func windowLoop() {
	var counter = 0
	for elem := range windowChannel {
		counter += 1
		ma.Add(float64(elem))
		if counter >= windowSize && ma.Avg() >= errorRate/100 {
			os.Exit(1)
		}
	}
}

func main() {
	flag.Parse()

	transport := &http.Transport{
		MaxIdleConns:    10,
		IdleConnTimeout: 10 * time.Second,
	}

	if sslSkipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	var inputReader io.Reader

	if debug {
		log.Printf("Parsing %s log file\n", inputLogFile)
		log.Printf("Using log type %s", inputFileType)
	}

	if inputLogFile == "dummy" {
		if inputFileType == "nginx" {
			inputReader = strings.NewReader(`89.234.89.123 [08/Nov/2013:13:39:18 +0000] "GET /t/100x100/foo/bar.jpeg HTTP/1.1" 200 1027 2430 0.014 "100x100" 10 1`)
		} else {
			inputReader = strings.NewReader(`<142>Sep 27 00:15:57 haproxy[28513]: 67.188.214.167:64531 [27/Sep/2013:00:15:43.494] frontend~ test/10.127.57.177-10000 449/0/0/13531/13980 200 13824 - - ---- 6/6/0/1/0 0/0 "GET / HTTP/1.1"`)
		}
	} else if inputLogFile == "-" {
		inputReader = os.Stdin
	} else {
		file, err := os.Open(inputLogFile)

		reader.Must(err)
		defer file.Close()

		if strings.HasSuffix(inputLogFile, "gz") {
			inputReader, err = gzip.NewReader(file)
			reader.Must(err)
		} else {
			inputReader = file
		}
	}

	var reader reader.LogReader

	switch inputFileType {
	case "nginx":
		reader = nginx.NewReader(inputReader, format)
	case "haproxy":
		reader = haproxy.NewReader(inputReader)
	case "solr":
		reader = solr.NewReader(inputReader)
	default:
		log.Fatalf("file-type can be either haproxy or nginx, not '%s'", inputFileType)
	}

	logWg.Add(1)
	go logLoop()

	if enableWindow {
		windowChannel = make(chan int8)
		ma = movavg.NewSMA(windowSize)
		go windowLoop()
		defer close(windowChannel)
	}

	mainLoop(reader, transport)

	if debug {
		log.Println("Waiting for all http goroutines to stop")
	}

	httpWg.Wait()
	close(logChannel)

	if debug {
		log.Println("Waiting for log goroutine to stop")
	}

	logWg.Wait()
}
