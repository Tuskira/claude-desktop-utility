// Command interceptor is a TLS-intercepting (MITM) proxy for inspecting the
// HTTP(S) traffic of an application, using a locally generated CA.
// It only observes traffic; it never modifies it.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/Tuskira/claude-desktop-utility/internal/ca"
	"github.com/Tuskira/claude-desktop-utility/internal/capture"
	"github.com/Tuskira/claude-desktop-utility/internal/claudedesktop"
	"github.com/Tuskira/claude-desktop-utility/internal/forward"
	"github.com/Tuskira/claude-desktop-utility/internal/protodec"
	"github.com/Tuskira/claude-desktop-utility/internal/proxy"
)

var errUsage = errors.New("usage")

const usage = `interceptor - TLS-intercepting proxy for inspecting an app's HTTP(S) traffic

Usage:
  interceptor ca init [--dir DIR] [--force]   create the local CA
  interceptor ca path [--dir DIR]             print the CA certificate path
  interceptor run [flags]                     start the proxy (requires --gateway-url and a gateway key)
  interceptor decode FILE.jsonl [--grep RE]   show decoded protobuf bodies from a capture file
  interceptor decode --raw BASE64|-           decode one base64 protobuf blob

Run "interceptor run -h" for the run flags. See deploy/macos/README.md for the
full install guide, including how to create a gateway key.
`

func main() {
	err := runCLI(os.Args[1:])
	switch {
	case err == nil:
	case errors.Is(err, errUsage):
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "interceptor:", err)
		os.Exit(1)
	}
}

func runCLI(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errUsage
	}
	switch args[0] {
	case "ca":
		if len(args) < 2 {
			fmt.Fprint(os.Stderr, usage)
			return errUsage
		}
		switch args[1] {
		case "init":
			return cmdCAInit(args[2:])
		case "path":
			return cmdCAPath(args[2:])
		}
		fmt.Fprint(os.Stderr, usage)
		return errUsage
	case "run":
		return cmdRun(args[1:])
	case "decode":
		return cmdDecode(args[1:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return nil
	}
	fmt.Fprint(os.Stderr, usage)
	return errUsage
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("interceptor "+name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

// parse parses args; -h yields flag.ErrHelp, other problems errUsage.
func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flag.ErrHelp
		}
		return errUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument %q\n", fs.Arg(0))
		return errUsage
	}
	return nil
}

func ignoreHelp(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

func defaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".interceptor"
	}
	return filepath.Join(home, ".interceptor")
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

func cmdCAInit(args []string) error {
	fs := newFlagSet("ca init")
	dir := fs.String("dir", defaultDir(), "CA directory")
	force := fs.Bool("force", false, "overwrite an existing CA")
	if err := parse(fs, args); err != nil {
		return ignoreHelp(err)
	}
	d := expandHome(*dir)
	if err := ca.Init(d, *force); err != nil {
		return err
	}
	printTrustHelp(os.Stdout, d)
	return nil
}

func cmdCAPath(args []string) error {
	fs := newFlagSet("ca path")
	dir := fs.String("dir", defaultDir(), "CA directory")
	if err := parse(fs, args); err != nil {
		return ignoreHelp(err)
	}
	fmt.Println(ca.CertPath(expandHome(*dir)))
	return nil
}

func printTrustHelp(w io.Writer, dir string) {
	cert := ca.CertPath(dir)
	fmt.Fprintf(w, `CA created:
  certificate: %s
  private key: %s  (keep it secret: anyone with it can intercept your TLS)

Trust it on macOS (system-wide):
  sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain %s

Apps with their own trust stores (Node/Electron, Python, curl builds, ...) need:
  export NODE_EXTRA_CA_CERTS=%s
  export SSL_CERT_FILE=%s
  export REQUESTS_CA_BUNDLE=%s
`, cert, ca.KeyPath(dir), cert, cert, cert, cert)
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func cmdRun(args []string) error {
	fs := newFlagSet("run")
	dir := fs.String("dir", defaultDir(), "CA directory (created if missing)")
	proxyAddr := fs.String("proxy-addr", "127.0.0.1:8080", "explicit HTTP proxy listen address (empty disables)")
	tlsAddr := fs.String("transparent-addr", "", "raw TLS listener for redirected traffic, e.g. 127.0.0.1:8443 (empty disables)")
	httpAddr := fs.String("transparent-http-addr", "", "raw plain-HTTP listener for redirected traffic (empty disables)")
	out := fs.String("out", "capture.jsonl", "JSONL capture file, appended to (empty disables)")
	maxBody := fs.String("max-body", "1MB", "max captured bytes per message body (e.g. 512KB, 2MB, 4096)")
	verbose := fs.Bool("v", false, "print headers and bodies, not just a summary line")
	noStream := fs.Bool("no-stream-records", false, "do not write one record per SSE event / RPC envelope / NDJSON line for streaming responses")
	insecure := fs.Bool("insecure-upstream", false, "skip upstream TLS verification (dev/tests only)")
	var filter, pass stringList
	fs.Var(&filter, "filter-host", "only log hosts matching this glob, e.g. *.anthropic.com (repeatable; everything is still proxied)")
	fs.Var(&pass, "passthrough", "host glob to tunnel without interception, for pinned apps (repeatable; matches host or host:port)")
	gatewayURL := fs.String("gateway-url", "", "gateway control-plane base URL, e.g. http://localhost:8081 (required; Claude Desktop traffic is forwarded there as gateway-shaped records — see deploy/macos/README.md Part 3)")
	gatewayKeyFile := fs.String("gateway-key-file", "", "path to a file holding the gateway key, mode 0600 (required unless INTERCEPTOR_GATEWAY_KEY is set — see deploy/macos/README.md Part 3)")
	spoolDir := fs.String("spool-dir", filepath.Join("~", ".interceptor", "spool"), "directory for records that could not be forwarded yet")
	forwardUser := fs.String("forward-user", "", "user label sent with forwarded records, overriding the Claude account email learned from traffic")
	if err := parse(fs, args); err != nil {
		return ignoreHelp(err)
	}
	if *proxyAddr == "" && *tlsAddr == "" && *httpAddr == "" {
		return errors.New("no listeners enabled")
	}
	// Gateway forwarding is not optional: this build only ever runs pointed
	// at a gateway. Fail fast, before any CA/capture-file side effects and
	// well before any listener binds a port, so a misconfigured launchd
	// service or terminal run never silently starts observing traffic with
	// nowhere for it to go.
	if strings.TrimSpace(*gatewayURL) == "" {
		return errors.New("interceptor run: --gateway-url is required (gateway forwarding cannot be turned off); see deploy/macos/README.md Part 3 to create a gateway key and set --gateway-url")
	}
	maxBytes, err := capture.ParseSize(*maxBody)
	if err != nil {
		return fmt.Errorf("--max-body: %w", err)
	}

	d := expandHome(*dir)
	if !ca.Exists(d) {
		if err := ca.Init(d, false); err != nil {
			return err
		}
		printTrustHelp(os.Stderr, d)
		fmt.Fprintln(os.Stderr)
	}
	authority, err := ca.Load(d)
	if err != nil {
		return err
	}

	w, err := capture.OpenWriter(*out)
	if err != nil {
		return err
	}
	defer w.Close()
	logger := &capture.Logger{Out: w, Stdout: os.Stdout, Verbose: *verbose, FilterHosts: filter}

	// Created here (rather than just before the final select below) because
	// the forwarder's background loop, started next, needs to stop on the
	// same signal that stops the proxy.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Claude Desktop -> gateway forwarding. --gateway-url
	// is required (checked above), so this always runs: every capture
	// record flows through claudedesktop.Converter (turns Code/Chat tab
	// traffic into gateway-shaped records) into forward.Forwarder (batches,
	// redacts, gzips, and POSTs them, spooling on failure).
	var conv *claudedesktop.Converter // assigned below; captured by UserLabel's closure
	fwd, err := forward.New(forward.Config{
		GatewayURL: *gatewayURL,
		KeyFile:    expandHome(*gatewayKeyFile),
		KeyEnv:     os.Getenv("INTERCEPTOR_GATEWAY_KEY"),
		SpoolDir:   expandHome(*spoolDir),
		OwnAddrs:   []string{*proxyAddr, *tlsAddr, *httpAddr},
		UserLabel: func() string {
			if conv == nil {
				return ""
			}
			return conv.UserLabel()
		},
		Logf: func(f string, a ...any) { fmt.Fprintf(os.Stderr, "interceptor: "+f+"\n", a...) },
	})
	if err != nil {
		return err
	}
	conv = claudedesktop.New(claudedesktop.Config{
		Sink:          fwd,
		ForwardUser:   *forwardUser,
		UserLabelFile: filepath.Join(d, "user-label"),
		Logf:          func(f string, a ...any) { fmt.Fprintf(os.Stderr, "interceptor: claudedesktop: "+f+"\n", a...) },
	})
	logger.Consumer = conv.Handle
	go fwd.Run(ctx)
	fmt.Fprintf(os.Stderr, "interceptor: forwarding Claude Desktop traffic to %s\n", *gatewayURL)

	srv := proxy.New(proxy.Config{
		CA:               authority,
		Logger:           logger,
		MaxBody:          maxBytes,
		Passthrough:      pass,
		InsecureUpstream: *insecure,
		NoStreamRecords:  *noStream,
		Logf:             func(f string, a ...any) { fmt.Fprintf(os.Stderr, "interceptor: "+f+"\n", a...) },
	})

	type listener struct {
		name, addr string
		serve      func(net.Listener) error
	}
	var lns []net.Listener
	errCh := make(chan error, 3)
	for _, l := range []listener{
		{"proxy", *proxyAddr, srv.ServeProxy},
		{"transparent TLS", *tlsAddr, srv.ServeTransparentTLS},
		{"transparent HTTP", *httpAddr, srv.ServeTransparentHTTP},
	} {
		if l.addr == "" {
			continue
		}
		ln, err := net.Listen("tcp", l.addr)
		if err != nil {
			for _, o := range lns {
				o.Close()
			}
			return fmt.Errorf("%s listener: %w", l.name, err)
		}
		lns = append(lns, ln)
		fmt.Fprintf(os.Stderr, "interceptor: %s listening on %s\n", l.name, ln.Addr())
		go func(serve func(net.Listener) error, ln net.Listener) { errCh <- serve(ln) }(l.serve, ln)
	}
	if *out != "" {
		fmt.Fprintf(os.Stderr, "interceptor: capturing to %s\n", *out)
	}

	var runErr error
	select {
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "interceptor: shutting down")
	case runErr = <-errCh:
	}
	srv.Shutdown(time.Second)
	if fwd != nil {
		stop()                     // in case we got here via errCh, not a signal
		fwd.Close(5 * time.Second) // best-effort final flush; unsent records are already safe on disk
	}
	return runErr
}

// cmdDecode prints the schema-less protobuf decoding of proto records in a
// capture file, or of a single base64 blob. Flags may follow the file name.
func cmdDecode(args []string) error {
	fs := newFlagSet("decode")
	grep := fs.String("grep", "", "only show records whose decoded text matches this regex")
	rawArg := fs.String("raw", "", "decode one base64 blob (\"-\" reads stdin) instead of a file")
	var files []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return errUsage
		}
		if fs.NArg() == 0 {
			break
		}
		files = append(files, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if *rawArg != "" {
		return decodeRaw(*rawArg)
	}
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "usage: interceptor decode FILE.jsonl [--grep RE] | --raw BASE64|-")
		return errUsage
	}
	var re *regexp.Regexp
	if *grep != "" {
		var err error
		if re, err = regexp.Compile(*grep); err != nil {
			return fmt.Errorf("--grep: %w", err)
		}
	}
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	for _, f := range files {
		if err := decodeFile(out, expandHome(f), re); err != nil {
			return err
		}
	}
	return nil
}

func decodeRaw(arg string) error {
	if arg == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		arg = string(b)
	}
	arg = strings.Join(strings.Fields(arg), "")
	var data []byte
	var err error
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if data, err = enc.DecodeString(arg); err == nil {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("--raw: not valid base64: %w", err)
	}
	js, msg := protodec.DecodeBody(protodec.KindMessage, data, false)
	if js == nil { // maybe a Connect/gRPC-Web stream
		if js2, msg2 := protodec.DecodeBody(protodec.KindStream, data, false); msg2 == "" && len(data) >= 5 && data[0] <= 0x83 {
			js, msg = js2, ""
		}
	}
	if js == nil {
		return errors.New(msg)
	}
	fmt.Println(indentJSON(js, ""))
	return nil
}

func indentJSON(js []byte, prefix string) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, js, prefix, "  "); err != nil {
		return prefix + string(js)
	}
	return prefix + buf.String()
}

func decodeFile(out io.Writer, path string, re *regexp.Regexp) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<20)
	for lineNo := 1; ; lineNo++ {
		line, rerr := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var rec capture.Record
			if err := json.Unmarshal(line, &rec); err != nil {
				fmt.Fprintf(os.Stderr, "interceptor: %s:%d: skipping bad line: %v\n", path, lineNo, err)
			} else {
				printDecoded(out, &rec, re)
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return nil
			}
			return rerr
		}
	}
}

func printDecoded(out io.Writer, rec *capture.Record, re *regexp.Regexp) {
	req, resp, errs := rec.Decoded()
	if rec.Mode == capture.ModeStream { // per-unit records already carry their decoded body
		req, resp, errs = nil, rec.RespBodyDecoded, ""
	}
	if req == nil && resp == nil {
		return
	}
	if re != nil && !re.Match(req) && !re.Match(resp) {
		return
	}
	seq := ""
	if rec.Seq > 0 {
		seq = fmt.Sprintf("  #%d", rec.Seq)
	}
	fmt.Fprintf(out, "%s  %s  %s%s\n", rec.TS.Format("2006-01-02T15:04:05.000Z07:00"), rec.Method, rec.URL, seq)
	for _, side := range []struct {
		name string
		js   []byte
	}{{"request", req}, {"response", resp}} {
		if side.js != nil {
			fmt.Fprintf(out, "  %s:\n%s\n", side.name, indentJSON(side.js, "    "))
		}
	}
	if errs != "" {
		fmt.Fprintf(out, "  (decode: %s)\n", errs)
	}
	fmt.Fprintln(out)
}
