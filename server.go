// server.go

package main

import (
	"bufio"			//buffered I/O - easily read lines for a conn
	"bytes"			//to read or write files we must create a byte buffer
	"errors"		//to build small reusable error values
	"fmt"			//formatting I/O
	"io"			//to I/O
	"log"			//to set up our logwriter
	"mime"			//to guess extensions
	"net"			//for creating listener and accepting connections
	"os"			//creating dir and stuff like that
	"path/filepath"	//combining requested path with the default path
	"strings"		//for splitting request lines and trimming CRLF
	"time"			//for timestamps
)

// ─────────────────────────────────────────────────────────────────
//  Configuration constants & globals
// ─────────────────────────────────────────────────────────────────

//DefaultRoot is the folder from which we serve static files
const DefaultRoot = "./public"

//DefaultListenAddr is the TCP address (host:port) our server will listen on.
//Since we didn't specify any host, our server would listen on all interfaces
const DefaultListenAddr = ":8080"

//logWriter is the global pointer to log.Logger that writes into a file. (in the log folder)
var logWriter *log.Logger

// ─────────────────────────────────────────────────────────────────
//  main()
//    - Parses flags (hardcoded for now, but you could use `flag` package).
//    - Sets up logging (writes to ./logs/server.log).
//    - Listens on TCP, accepts connections, spawns handleConnection().
// ─────────────────────────────────────────────────────────────────

func main() {
	//Prepare the logs directory (./logs/server.log)
	err := os.MkdirAll("logs", 0755)
	if err != nil {
		fmt.Printf("Could not create logs directory: %v\n", err)
		os.Exit(1)
	}

	//Open (or create) ./logs/server.log for appending
	logFile, err := os.OpenFile("logs/server.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644) //flags to append and create the file if not existing
	if err != nil {
		fmt.Printf("Could not open log file: %v\n", err)
		os.Exit(1)
	}
	defer logFile.Close() //close the logger after the main function returns

	//Create a new logger that writes to logFile, with no default prefix
	//We’ll add our own prefixes manually (like. "[INFO]")
	logWriter = log.New(logFile, "", 0)

	//Log server startup - message to both log and stdout
	startupMsg := fmt.Sprintf("[INFO] %s – Server starting on %s\n", time.Now().UTC().Format(time.RFC3339), DefaultListenAddr)
	logWriter.Print(startupMsg)
	fmt.Print(startupMsg)

	//Create a TCP listener
	listener, err := net.Listen("tcp", DefaultListenAddr)
	if err != nil {
		logWriter.Printf("[ERROR] %s – Could not listen on %s: %v\n", time.Now().UTC().Format(time.RFC3339), DefaultListenAddr, err)
		fmt.Printf("Could not listen on %s: %v\n", DefaultListenAddr, err)
		os.Exit(1)
	}
	//Logging when the server closes the connection
	defer func() {
		listener.Close()
		shutdownMsg := fmt.Sprintf("[INFO] %s – Server shutting down\n", time.Now().UTC().Format(time.RFC3339))
		logWriter.Print(shutdownMsg)
		fmt.Print(shutdownMsg)
	}()

	//infinte loop for multiple clients
	//for each connecttion, start a goroutine
	for {
		conn, err := listener.Accept()
		if err != nil {
			//accept failure gets logged
			logWriter.Printf("[ERROR] %s – Accept error: %v\n", time.Now().UTC().Format(time.RFC3339), err)
			continue
		}
		//new connection (custom function) 
		go handleConnection(conn)
	}
}

// ─────────────────────────────────────────────────────────────────
//  handleConnection()
//    - Reads the request line (e.g. "GET /foo/bar.html HTTP/1.1").
//    - Reads & discards the rest of the request headers.
//    - Figures out which file on disk to serve.
//    - Checks existence/permissions.
//    - Determines content‐type (MIME).
//    - Writes either: 200 + file contents, OR 403/404 + custom error page.
//    - Logs each request in the desired format.
// ─────────────────────────────────────────────────────────────────

func handleConnection(conn net.Conn) {
	defer conn.Close() //close connection when the function returns

	//stores client address in string format
	clientAddr := conn.RemoteAddr().String() // e.g. "127.0.0.1:51748" 

	//thise create a buffered reader object which when called to read
	//first reads from the buffer and when it's expty only then makes 
	//a call to conn. This way the number of calls are minimized thus
	//offering much greater efficiency than calling conn everytime
	reader := bufio.NewReader(conn)

	//Read the request line (method, path, version)
	requestLine, err := readRequestLine(reader) // custom function
	if err != nil {
		// If we couldn’t read a valid request line, close silently.
		return
	}
	//example of a requestLine: "GET /index.html HTTP/1.1"
	parts := strings.Split(requestLine, " ")
	if len(parts) != 3 {
		// Malformed request line → ignore or could write a 400 Bad Request. We’ll just close.
		return
	}
	method, rawPath, version := parts[0], parts[1], parts[2]

	//Discard all remaining request headers until a blank line
	err = discardHeaders(reader)
	if err != nil {
		// If something goes wrong reading headers, just close.
		return
	}

	// We only support GET. If anything else, respond 405 Method Not Allowed.
	if method != "GET" {
		statusLine := fmt.Sprintf("%s 405 Method Not Allowed\r\n", version)
		body := "<html><body><h1>405 Method Not Allowed</h1></body></html>"
		writeMinimalResponse(conn, statusLine, "text/html", []byte(body)) //custom function
		logRequest(clientAddr, requestLine, 405) //custom function for logging each request
		return
	}

	//Sanitize the requested path to prevent directory‐traversal
	//For example, if rawPath = "/../etc/passwd" we want to reject it.
	cleanPath, securityErr := sanitizePath(rawPath)
	if securityErr != nil {
		// Send 403 Forbidden if the path contained ".." or null bytes
		serveErrorPage(conn, clientAddr, requestLine, 403)
		return
	}

	// At this point, cleanPath is something like "/index.html" or "/css/style.css".
	// We want to map it to a file under DefaultRoot.
	localPath := filepath.Join(DefaultRoot, cleanPath)

	//Stat the file (or directory)
	info, err := os.Stat(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			// 404 Not Found
			serveErrorPage(conn, clientAddr, requestLine, 404)
		} else {
			// Some other error (e.g. 403)
			logWriter.Printf("[ERROR] %s – Stat error on %s: %v\n", time.Now().UTC().Format(time.RFC3339), localPath, err)
			serveErrorPage(conn, clientAddr, requestLine, 403)
		}
		return
	}

	//If it’s a directory, try to serve index.html inside
	if info.IsDir() {
		// Ensure the path ends in "/". If not, a browser might get confused, but
		// for simplicity we assume the client already asked for "/some/dir/".
		if !strings.HasSuffix(localPath, string(os.PathSeparator)) {
			localPath += string(os.PathSeparator)
		}
		indexPath := filepath.Join(localPath, "index.html")
		indexInfo, err := os.Stat(indexPath)
		if err != nil || indexInfo.IsDir() {
			// No index.html or cannot read → 403 Forbidden
			serveErrorPage(conn, clientAddr, requestLine, 403)
			return
		}
		// If we found a valid index.html, serve that file instead:
		localPath = indexPath
	}

	//At this point, localPath points to a regular file we intend to serve.
	//Open the file
	file, err := os.Open(localPath)
	if err != nil {
		// Permission denied or other error → 403
		logWriter.Printf("[ERROR] %s – Open error on %s: %v\n", time.Now().UTC().Format(time.RFC3339), localPath, err)
		serveErrorPage(conn, clientAddr, requestLine, 403)
		return
	}
	defer file.Close()

	//Determine Content‐Type (MIME) by extension
	ctype := detectContentType(localPath)

	//Read file content into memory (for small files) or stream
	//For simplicity, we’ll read the entire file before writing headers.
	buf := bytes.Buffer{}
	n, err := io.Copy(&buf, file)
	if err != nil {
		logWriter.Printf("[ERROR] %s – Read error on %s: %v\n", time.Now().UTC().Format(time.RFC3339), localPath, err)
		serveErrorPage(conn, clientAddr, requestLine, 500)
		return
	}

	//Write the HTTP/1.1 200 OK response
	statusLine := fmt.Sprintf("%s 200 OK\r\n", version)
	headers := fmt.Sprintf(
		"Date: %s\r\nContent-Type: %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n",
		time.Now().UTC().Format(time.RFC1123),
		ctype,
		n,
	)
	_, err = conn.Write([]byte(statusLine + headers))
	if err != nil {
		//If we can’t even write, return
		return
	}

	//Write the body (file contents)
	_, err = conn.Write(buf.Bytes())
	if err != nil {
		//If body writing fails, retunr
		return
	}

	//Log the successful request
	logRequest(clientAddr, requestLine, 200)
}

// ─────────────────────────────────────────────────────────────────
//  readRequestLine()
//    - Reads a single line from bufio.Reader (up to CRLF).
//    - Returns the line without the trailing CRLF.
// ─────────────────────────────────────────────────────────────────

func readRequestLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	// HTTP request lines end with "\r\n", so TrimRight ensures we remove both.
	return strings.TrimRight(line, "\r\n"), nil
}

// ─────────────────────────────────────────────────────────────────
//  discardHeaders()
//    - After reading the request line, an HTTP client will send
//      zero or more header lines, each ending in CRLF, then a blank line.
//    - We loop until we hit a blank line (\r\n) to know headers are done.
// ─────────────────────────────────────────────────────────────────

func discardHeaders(r *bufio.Reader) error {
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return err
		}
		// A blank line is just "\r\n"
		if line == "\r\n" {
			return nil
		}
		// Otherwise, keep looping (we’re ignoring header contents).
	}
}

// ─────────────────────────────────────────────────────────────────
//  sanitizePath(rawPath string) (cleanPath string, err error)
//    - Prevent directory‐traversal attacks.
//    - Ensure there are no “..” elements or null bytes in the path.
//    - Return the “cleaned” path, which always starts with "/".
// ─────────────────────────────────────────────────────────────────

// the client may access undesired file using .. which takes to the parent directory
func sanitizePath(rawPath string) (string, error) {
	//Reject null bytes immediately
	if strings.Contains(rawPath, "\x00") {
		return "", errors.New("null byte in path")
	}
	//Clean up path (this collapses “/foo/../bar” into “/bar”)
	cleaned := filepath.Clean(rawPath)
	//Ensure it still begins with “/”
	if !strings.HasPrefix(cleaned, "/") {
		return "", errors.New("invalid path")
	}
	//Prevent any “..” after cleaning (filepath.Clean can collapse, but if
	//someone tried “/../../etc/passwd”, Clean would return “/etc/passwd”).
	//As long as we take “/etc/passwd”, our Join(DefaultRoot, "/etc/passwd")
	//would actually escape the root. So a safer check is to see if cleaned
	//has “….” after splitting.
	for _, segment := range strings.Split(cleaned, "/") {
		if segment == ".." {
			return "", errors.New("path traversal attempt")
		}
	}
	return cleaned, nil
}

// ─────────────────────────────────────────────────────────────────
//  detectContentType(filePath string) string
//    - Uses mime.TypeByExtension to guess a Content‐Type from extension.
//    - Falls back to "application/octet-stream" if unknown.
// ─────────────────────────────────────────────────────────────────

func detectContentType(filePath string) string {

	//octet stream is used for unknown file types

	ext := strings.ToLower(filepath.Ext(filePath))
	if ext == "" {
		return "application/octet-stream"
	}
	ctype := mime.TypeByExtension(ext)
	if ctype == "" {
		return "application/octet-stream"
	}
	return ctype
}

// ─────────────────────────────────────────────────────────────────
//  serveErrorPage()
//    - Depending on the status code (403 or 404), we try to serve
//      public/403.html or public/404.html. If that file is missing,
//      we write a minimal default HTML body.
//    - We then log the request with the status code.
// ─────────────────────────────────────────────────────────────────

func serveErrorPage(conn net.Conn, clientAddr, requestLine string, statusCode int) {
	version := "HTTP/1.1"
	var statusText string
	var errorFile string

	switch statusCode {
	case 403:
		statusText = "403 Forbidden"
		errorFile = filepath.Join(DefaultRoot, "403.html")
	case 404:
		statusText = "404 Not Found"
		errorFile = filepath.Join(DefaultRoot, "404.html")
	default:
		statusText = fmt.Sprintf("%d Error", statusCode)
		errorFile = "" // no custom page
	}

	// Attempt to read the custom error HTML from disk
	var bodyBytes []byte
	if errorFile != "" {
		data, err := os.ReadFile(errorFile)
		if err == nil {
			bodyBytes = data
		}
	}

	// If we couldn’t read the custom page, fall back to a minimal built‐in body
	if bodyBytes == nil {
		fallback := fmt.Sprintf("<html><body><h1>%s</h1></body></html>", statusText)
		bodyBytes = []byte(fallback)
	}

	// Write response headers + body
	statusLine := fmt.Sprintf("%s %s\r\n", version, statusText)
	headers := fmt.Sprintf(
		"Date: %s\r\nContent-Type: text/html\r\nContent-Length: %d\r\nConnection: close\r\n\r\n",
		time.Now().UTC().Format(time.RFC1123),
		len(bodyBytes),
	)
	_, _ = conn.Write([]byte(statusLine + headers))
	_, _ = conn.Write(bodyBytes)

	// Log the request with status code
	logRequest(clientAddr, requestLine, statusCode)
}

// ─────────────────────────────────────────────────────────────────
//  writeMinimalResponse()
//    - For tiny/manual responses (like 405 Method Not Allowed), we
//      can write a minimal status line + headers + body.
// ─────────────────────────────────────────────────────────────────

func writeMinimalResponse(conn net.Conn, statusLine, contentType string, body []byte) {
	headers := fmt.Sprintf(
		"Date: %s\r\nContent-Type: %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n",
		time.Now().UTC().Format(time.RFC1123),
		contentType,
		len(body),
	)
	conn.Write([]byte(statusLine + headers))
	conn.Write(body)
}

// ─────────────────────────────────────────────────────────────────
//  logRequest()
//    - Writes a single line to our log file in this format:
//      [INFO] <timestamp> – <client_ip>:<port> – "<METHOD PATH HTTP/VERSION>" – <STATUS_CODE>
// ─────────────────────────────────────────────────────────────────

func logRequest(clientAddr, requestLine string, statusCode int) {
	ts := time.Now().UTC().Format(time.RFC3339)
	logEntry := fmt.Sprintf("[INFO] %s – %s – %q – %d\n", ts, clientAddr, requestLine, statusCode)
	logWriter.Print(logEntry)
}
