package server

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/emersion/go-smtp"
	"github.com/gorilla/websocket"
	"golang.org/x/sync/errgroup"
	"heckel.io/ntfy/auth"
	"heckel.io/ntfy/util"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Server is the main server, providing the UI and API for ntfy
type Server struct {
	config       *Config
	httpServer   *http.Server
	httpsServer  *http.Server
	unixListener net.Listener
	smtpServer   *smtp.Server
	smtpBackend  *smtpBackend
	topics       map[string]*topic
	visitors     map[string]*visitor
	firebase     subscriber
	mailer       mailer
	messages     int64
	auth         auth.Auther
	messageCache *messageCache
	fileCache    *fileCache
	closeChan    chan bool
	mu           sync.Mutex
}

// handleFunc extends the normal http.HandlerFunc to be able to easily return errors
type handleFunc func(http.ResponseWriter, *http.Request, *visitor) error

var (
	// If changed, don't forget to update Android App and auth_sqlite.go
	topicRegex             = regexp.MustCompile(`^[-_A-Za-z0-9]{1,64}$`)               // No /!
	topicPathRegex         = regexp.MustCompile(`^/[-_A-Za-z0-9]{1,64}$`)              // Regex must match JS & Android app!
	externalTopicPathRegex = regexp.MustCompile(`^/[^/]+\.[^/]+/[-_A-Za-z0-9]{1,64}$`) // Extended topic path, for web-app, e.g. /example.com/mytopic
	jsonPathRegex          = regexp.MustCompile(`^/[-_A-Za-z0-9]{1,64}(,[-_A-Za-z0-9]{1,64})*/json$`)
	ssePathRegex           = regexp.MustCompile(`^/[-_A-Za-z0-9]{1,64}(,[-_A-Za-z0-9]{1,64})*/sse$`)
	rawPathRegex           = regexp.MustCompile(`^/[-_A-Za-z0-9]{1,64}(,[-_A-Za-z0-9]{1,64})*/raw$`)
	wsPathRegex            = regexp.MustCompile(`^/[-_A-Za-z0-9]{1,64}(,[-_A-Za-z0-9]{1,64})*/ws$`)
	authPathRegex          = regexp.MustCompile(`^/[-_A-Za-z0-9]{1,64}(,[-_A-Za-z0-9]{1,64})*/auth$`)
	publishPathRegex       = regexp.MustCompile(`^/[-_A-Za-z0-9]{1,64}/(publish|send|trigger)$`)

	webConfigPath    = "/config.js"
	userStatsPath    = "/user/stats"
	staticRegex      = regexp.MustCompile(`^/static/.+`)
	docsRegex        = regexp.MustCompile(`^/docs(|/.*)$`)
	fileRegex        = regexp.MustCompile(`^/file/([-_A-Za-z0-9]{1,64})(?:\.[A-Za-z0-9]{1,16})?$`)
	disallowedTopics = []string{"docs", "static", "file", "app", "settings"} // If updated, also update in Android app
	attachURLRegex   = regexp.MustCompile(`^https?://`)

	//go:embed "example.html"
	exampleSource string

	//go:embed site
	webFs        embed.FS
	webFsCached  = &util.CachingEmbedFS{ModTime: time.Now(), FS: webFs}
	webSiteDir   = "/site"
	webHomeIndex = "/home.html" // Landing page, only if "web-root: home"
	webAppIndex  = "/app.html"  // React app

	//go:embed docs
	docsStaticFs     embed.FS
	docsStaticCached = &util.CachingEmbedFS{ModTime: time.Now(), FS: docsStaticFs}
)

const (
	firebaseControlTopic     = "~control"                // See Android if changed
	emptyMessageBody         = "triggered"               // Used if message body is empty
	defaultAttachmentMessage = "You received a file: %s" // Used if message body is empty, and there is an attachment
	encodingBase64           = "base64"
)

// WebSocket constants
const (
	wsWriteWait  = 2 * time.Second
	wsBufferSize = 1024
	wsReadLimit  = 64 // We only ever receive PINGs
	wsPongWait   = 15 * time.Second
)

// New instantiates a new Server. It creates the cache and adds a Firebase
// subscriber (if configured).
func New(conf *Config) (*Server, error) {
	var mailer mailer
	if conf.SMTPSenderAddr != "" {
		mailer = &smtpSender{config: conf}
	}
	messageCache, err := createMessageCache(conf)
	if err != nil {
		return nil, err
	}
	topics, err := messageCache.Topics()
	if err != nil {
		return nil, err
	}
	var fileCache *fileCache
	if conf.AttachmentCacheDir != "" {
		fileCache, err = newFileCache(conf.AttachmentCacheDir, conf.AttachmentTotalSizeLimit, conf.AttachmentFileSizeLimit)
		if err != nil {
			return nil, err
		}
	}
	var auther auth.Auther
	if conf.AuthFile != "" {
		auther, err = auth.NewSQLiteAuth(conf.AuthFile, conf.AuthDefaultRead, conf.AuthDefaultWrite)
		if err != nil {
			return nil, err
		}
	}
	var firebaseSubscriber subscriber
	if conf.FirebaseKeyFile != "" {
		var err error
		firebaseSubscriber, err = createFirebaseSubscriber(conf.FirebaseKeyFile, auther)
		if err != nil {
			return nil, err
		}
	}
	return &Server{
		config:       conf,
		messageCache: messageCache,
		fileCache:    fileCache,
		firebase:     firebaseSubscriber,
		mailer:       mailer,
		topics:       topics,
		auth:         auther,
		visitors:     make(map[string]*visitor),
	}, nil
}

func createMessageCache(conf *Config) (*messageCache, error) {
	if conf.CacheDuration == 0 {
		return newNopCache()
	} else if conf.CacheFile != "" {
		return newSqliteCache(conf.CacheFile, false)
	}
	return newMemCache()
}

// Run executes the main server. It listens on HTTP (+ HTTPS, if configured), and starts
// a manager go routine to print stats and prune messages.
func (s *Server) Run() error {
	var listenStr string
	if s.config.ListenHTTP != "" {
		listenStr += fmt.Sprintf(" %s[http]", s.config.ListenHTTP)
	}
	if s.config.ListenHTTPS != "" {
		listenStr += fmt.Sprintf(" %s[https]", s.config.ListenHTTPS)
	}
	if s.config.ListenUnix != "" {
		listenStr += fmt.Sprintf(" %s[unix]", s.config.ListenUnix)
	}
	if s.config.SMTPServerListen != "" {
		listenStr += fmt.Sprintf(" %s[smtp]", s.config.SMTPServerListen)
	}
	log.Printf("Listening on%s", listenStr)
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	errChan := make(chan error)
	s.mu.Lock()
	s.closeChan = make(chan bool)
	if s.config.ListenHTTP != "" {
		s.httpServer = &http.Server{Addr: s.config.ListenHTTP, Handler: mux}
		go func() {
			errChan <- s.httpServer.ListenAndServe()
		}()
	}
	if s.config.ListenHTTPS != "" {
		s.httpsServer = &http.Server{Addr: s.config.ListenHTTPS, Handler: mux}
		go func() {
			errChan <- s.httpsServer.ListenAndServeTLS(s.config.CertFile, s.config.KeyFile)
		}()
	}
	if s.config.ListenUnix != "" {
		go func() {
			var err error
			s.mu.Lock()
			os.Remove(s.config.ListenUnix)
			s.unixListener, err = net.Listen("unix", s.config.ListenUnix)
			if err != nil {
				errChan <- err
				return
			}
			s.mu.Unlock()
			httpServer := &http.Server{Handler: mux}
			errChan <- httpServer.Serve(s.unixListener)
		}()
	}
	if s.config.SMTPServerListen != "" {
		go func() {
			errChan <- s.runSMTPServer()
		}()
	}
	s.mu.Unlock()
	go s.runManager()
	go s.runAtSender()
	go s.runFirebaseKeepaliver()

	return <-errChan
}

// Stop stops HTTP (+HTTPS) server and all managers
func (s *Server) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.httpServer != nil {
		s.httpServer.Close()
	}
	if s.httpsServer != nil {
		s.httpsServer.Close()
	}
	if s.unixListener != nil {
		s.unixListener.Close()
	}
	if s.smtpServer != nil {
		s.smtpServer.Close()
	}
	close(s.closeChan)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	v := s.visitor(r)
	if err := s.handleInternal(w, r, v); err != nil {
		if websocket.IsWebSocketUpgrade(r) {
			log.Printf("[%s] WS %s %s - %s", v.ip, r.Method, r.URL.Path, err.Error())
			return // Do not attempt to write to upgraded connection
		}
		httpErr, ok := err.(*errHTTP)
		if !ok {
			httpErr = errHTTPInternalError
		}
		log.Printf("[%s] HTTP %s %s - %d - %d - %s", v.ip, r.Method, r.URL.Path, httpErr.HTTPCode, httpErr.Code, err.Error())
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*") // CORS, allow cross-origin requests
		w.WriteHeader(httpErr.HTTPCode)
		io.WriteString(w, httpErr.JSON()+"\n")
	}
}

func (s *Server) handleInternal(w http.ResponseWriter, r *http.Request, v *visitor) error {
	if r.Method == http.MethodGet && r.URL.Path == "/" {
		return s.handleHome(w, r)
	} else if r.Method == http.MethodGet && r.URL.Path == "/example.html" {
		return s.handleExample(w, r)
	} else if r.Method == http.MethodHead && r.URL.Path == "/" {
		return s.handleEmpty(w, r, v)
	} else if r.Method == http.MethodGet && r.URL.Path == webConfigPath {
		return s.handleWebConfig(w, r)
	} else if r.Method == http.MethodGet && r.URL.Path == userStatsPath {
		return s.handleUserStats(w, r, v)
	} else if r.Method == http.MethodGet && staticRegex.MatchString(r.URL.Path) {
		return s.handleStatic(w, r)
	} else if r.Method == http.MethodGet && docsRegex.MatchString(r.URL.Path) {
		return s.handleDocs(w, r)
	} else if r.Method == http.MethodGet && fileRegex.MatchString(r.URL.Path) && s.config.AttachmentCacheDir != "" {
		return s.limitRequests(s.handleFile)(w, r, v)
	} else if r.Method == http.MethodOptions {
		return s.handleOptions(w, r)
	} else if (r.Method == http.MethodPut || r.Method == http.MethodPost) && r.URL.Path == "/" {
		return s.limitRequests(s.transformBodyJSON(s.authWrite(s.handlePublish)))(w, r, v)
	} else if (r.Method == http.MethodPut || r.Method == http.MethodPost) && topicPathRegex.MatchString(r.URL.Path) {
		return s.limitRequests(s.authWrite(s.handlePublish))(w, r, v)
	} else if r.Method == http.MethodGet && publishPathRegex.MatchString(r.URL.Path) {
		return s.limitRequests(s.authWrite(s.handlePublish))(w, r, v)
	} else if r.Method == http.MethodGet && jsonPathRegex.MatchString(r.URL.Path) {
		return s.limitRequests(s.authRead(s.handleSubscribeJSON))(w, r, v)
	} else if r.Method == http.MethodGet && ssePathRegex.MatchString(r.URL.Path) {
		return s.limitRequests(s.authRead(s.handleSubscribeSSE))(w, r, v)
	} else if r.Method == http.MethodGet && rawPathRegex.MatchString(r.URL.Path) {
		return s.limitRequests(s.authRead(s.handleSubscribeRaw))(w, r, v)
	} else if r.Method == http.MethodGet && wsPathRegex.MatchString(r.URL.Path) {
		return s.limitRequests(s.authRead(s.handleSubscribeWS))(w, r, v)
	} else if r.Method == http.MethodGet && authPathRegex.MatchString(r.URL.Path) {
		return s.limitRequests(s.authRead(s.handleTopicAuth))(w, r, v)
	} else if r.Method == http.MethodGet && (topicPathRegex.MatchString(r.URL.Path) || externalTopicPathRegex.MatchString(r.URL.Path)) {
		return s.handleTopic(w, r)
	}
	return errHTTPNotFound
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) error {
	if s.config.WebRootIsApp {
		r.URL.Path = webAppIndex
	} else {
		r.URL.Path = webHomeIndex
	}
	return s.handleStatic(w, r)
}

func (s *Server) handleTopic(w http.ResponseWriter, r *http.Request) error {
	unifiedpush := readBoolParam(r, false, "x-unifiedpush", "unifiedpush", "up") // see PUT/POST too!
	if unifiedpush {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*") // CORS, allow cross-origin requests
		_, err := io.WriteString(w, `{"unifiedpush":{"version":1}}`+"\n")
		return err
	}
	r.URL.Path = webAppIndex
	return s.handleStatic(w, r)
}

func (s *Server) handleEmpty(_ http.ResponseWriter, _ *http.Request, _ *visitor) error {
	return nil
}

func (s *Server) handleTopicAuth(w http.ResponseWriter, _ *http.Request, _ *visitor) error {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*") // CORS, allow cross-origin requests
	_, err := io.WriteString(w, `{"success":true}`+"\n")
	return err
}

func (s *Server) handleExample(w http.ResponseWriter, _ *http.Request) error {
	_, err := io.WriteString(w, exampleSource)
	return err
}

func (s *Server) handleWebConfig(w http.ResponseWriter, r *http.Request) error {
	appRoot := "/"
	if !s.config.WebRootIsApp {
		appRoot = "/app"
	}
	disallowedTopicsStr := `"` + strings.Join(disallowedTopics, `", "`) + `"`
	w.Header().Set("Content-Type", "text/javascript")
	_, err := io.WriteString(w, fmt.Sprintf(`// Generated server configuration
var config = {
  appRoot: "%s",
  disallowedTopics: [%s]
};`, appRoot, disallowedTopicsStr))
	return err
}

func (s *Server) handleUserStats(w http.ResponseWriter, r *http.Request, v *visitor) error {
	stats, err := v.Stats()
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/json")
	w.Header().Set("Access-Control-Allow-Origin", "*") // CORS, allow cross-origin requests
	if err := json.NewEncoder(w).Encode(stats); err != nil {
		return err
	}
	return nil
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) error {
	r.URL.Path = webSiteDir + r.URL.Path
	util.Gzip(http.FileServer(http.FS(webFsCached))).ServeHTTP(w, r)
	return nil
}

func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) error {
	util.Gzip(http.FileServer(http.FS(docsStaticCached))).ServeHTTP(w, r)
	return nil
}

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request, v *visitor) error {
	if s.config.AttachmentCacheDir == "" {
		return errHTTPInternalError
	}
	matches := fileRegex.FindStringSubmatch(r.URL.Path)
	if len(matches) != 2 {
		return errHTTPInternalErrorInvalidFilePath
	}
	messageID := matches[1]
	file := filepath.Join(s.config.AttachmentCacheDir, messageID)
	stat, err := os.Stat(file)
	if err != nil {
		return errHTTPNotFound
	}
	if err := v.BandwidthLimiter().Allow(stat.Size()); err != nil {
		return errHTTPTooManyRequestsAttachmentBandwidthLimit
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", stat.Size()))
	w.Header().Set("Access-Control-Allow-Origin", "*") // CORS, allow cross-origin requests
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(util.NewContentTypeWriter(w, r.URL.Path), f)
	return err
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request, v *visitor) error {
	t, err := s.topicFromPath(r.URL.Path)
	if err != nil {
		return err
	}
	body, err := util.Peek(r.Body, s.config.MessageLimit)
	if err != nil {
		return err
	}
	m := newDefaultMessage(t.ID, "")
	cache, firebase, email, unifiedpush, err := s.parsePublishParams(r, v, m)
	if err != nil {
		return err
	}
	if err := s.handlePublishBody(r, v, m, body, unifiedpush); err != nil {
		return err
	}
	if m.Message == "" {
		m.Message = emptyMessageBody
	}
	delayed := m.Time > time.Now().Unix()
	if !delayed {
		if err := t.Publish(m); err != nil {
			return err
		}
	}
	if s.firebase != nil && firebase && !delayed {
		go func() {
			if err := s.firebase(m); err != nil {
				log.Printf("[%s] FB - Unable to publish to Firebase: %v", v.ip, err.Error())
			}
		}()
	}
	if s.mailer != nil && email != "" && !delayed {
		go func() {
			if err := s.mailer.Send(v.ip, email, m); err != nil {
				log.Printf("[%s] MAIL - Unable to send email: %v", v.ip, err.Error())
			}
		}()
	}
	if cache {
		if err := s.messageCache.AddMessage(m); err != nil {
			return err
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*") // CORS, allow cross-origin requests
	if err := json.NewEncoder(w).Encode(m); err != nil {
		return err
	}
	s.mu.Lock()
	s.messages++
	s.mu.Unlock()
	return nil
}

func (s *Server) parsePublishParams(r *http.Request, v *visitor, m *message) (cache bool, firebase bool, email string, unifiedpush bool, err error) {
	cache = readBoolParam(r, true, "x-cache", "cache")
	firebase = readBoolParam(r, true, "x-firebase", "firebase")
	m.Title = readParam(r, "x-title", "title", "t")
	m.Click = readParam(r, "x-click", "click")
	filename := readParam(r, "x-filename", "filename", "file", "f")
	attach := readParam(r, "x-attach", "attach", "a")
	if attach != "" || filename != "" {
		m.Attachment = &attachment{}
	}
	if filename != "" {
		m.Attachment.Name = filename
	}
	if attach != "" {
		if !attachURLRegex.MatchString(attach) {
			return false, false, "", false, errHTTPBadRequestAttachmentURLInvalid
		}
		m.Attachment.URL = attach
		if m.Attachment.Name == "" {
			u, err := url.Parse(m.Attachment.URL)
			if err == nil {
				m.Attachment.Name = path.Base(u.Path)
				if m.Attachment.Name == "." || m.Attachment.Name == "/" {
					m.Attachment.Name = ""
				}
			}
		}
		if m.Attachment.Name == "" {
			m.Attachment.Name = "attachment"
		}
	}
	email = readParam(r, "x-email", "x-e-mail", "email", "e-mail", "mail", "e")
	if email != "" {
		if err := v.EmailAllowed(); err != nil {
			return false, false, "", false, errHTTPTooManyRequestsLimitEmails
		}
	}
	if s.mailer == nil && email != "" {
		return false, false, "", false, errHTTPBadRequestEmailDisabled
	}
	messageStr := strings.ReplaceAll(readParam(r, "x-message", "message", "m"), "\\n", "\n")
	if messageStr != "" {
		m.Message = messageStr
	}
	m.Priority, err = util.ParsePriority(readParam(r, "x-priority", "priority", "prio", "p"))
	if err != nil {
		return false, false, "", false, errHTTPBadRequestPriorityInvalid
	}
	tagsStr := readParam(r, "x-tags", "tags", "tag", "ta")
	if tagsStr != "" {
		m.Tags = make([]string, 0)
		for _, s := range util.SplitNoEmpty(tagsStr, ",") {
			m.Tags = append(m.Tags, strings.TrimSpace(s))
		}
	}
	delayStr := readParam(r, "x-delay", "delay", "x-at", "at", "x-in", "in")
	if delayStr != "" {
		if !cache {
			return false, false, "", false, errHTTPBadRequestDelayNoCache
		}
		if email != "" {
			return false, false, "", false, errHTTPBadRequestDelayNoEmail // we cannot store the email address (yet)
		}
		delay, err := util.ParseFutureTime(delayStr, time.Now())
		if err != nil {
			return false, false, "", false, errHTTPBadRequestDelayCannotParse
		} else if delay.Unix() < time.Now().Add(s.config.MinDelay).Unix() {
			return false, false, "", false, errHTTPBadRequestDelayTooSmall
		} else if delay.Unix() > time.Now().Add(s.config.MaxDelay).Unix() {
			return false, false, "", false, errHTTPBadRequestDelayTooLarge
		}
		m.Time = delay.Unix()
	}
	actionsStr := readParam(r, "x-actions", "actions", "action")
	if actionsStr != "" {
		m.Actions, err = parseActions(actionsStr)
		if err != nil {
			return false, false, "", false, wrapErrHTTP(errHTTPBadRequestActionsInvalid, err.Error())
		}
	}
	unifiedpush = readBoolParam(r, false, "x-unifiedpush", "unifiedpush", "up") // see GET too!
	if unifiedpush {
		firebase = false
		unifiedpush = true
	}
	return cache, firebase, email, unifiedpush, nil
}

// handlePublishBody consumes the PUT/POST body and decides whether the body is an attachment or the message.
//
// 1. curl -T somebinarydata.bin "ntfy.sh/mytopic?up=1"
//    If body is binary, encode as base64, if not do not encode
// 2. curl -H "Attach: http://example.com/file.jpg" ntfy.sh/mytopic
//    Body must be a message, because we attached an external URL
// 3. curl -T short.txt -H "Filename: short.txt" ntfy.sh/mytopic
//    Body must be attachment, because we passed a filename
// 4. curl -T file.txt ntfy.sh/mytopic
//    If file.txt is <= 4096 (message limit) and valid UTF-8, treat it as a message
// 5. curl -T file.txt ntfy.sh/mytopic
//    If file.txt is > message limit, treat it as an attachment
func (s *Server) handlePublishBody(r *http.Request, v *visitor, m *message, body *util.PeekedReadCloser, unifiedpush bool) error {
	if unifiedpush {
		return s.handleBodyAsMessageAutoDetect(m, body) // Case 1
	} else if m.Attachment != nil && m.Attachment.URL != "" {
		return s.handleBodyAsTextMessage(m, body) // Case 2
	} else if m.Attachment != nil && m.Attachment.Name != "" {
		return s.handleBodyAsAttachment(r, v, m, body) // Case 3
	} else if !body.LimitReached && utf8.Valid(body.PeekedBytes) {
		return s.handleBodyAsTextMessage(m, body) // Case 4
	}
	return s.handleBodyAsAttachment(r, v, m, body) // Case 5
}

func (s *Server) handleBodyAsMessageAutoDetect(m *message, body *util.PeekedReadCloser) error {
	if utf8.Valid(body.PeekedBytes) {
		m.Message = string(body.PeekedBytes) // Do not trim
	} else {
		m.Message = base64.StdEncoding.EncodeToString(body.PeekedBytes)
		m.Encoding = encodingBase64
	}
	return nil
}

func (s *Server) handleBodyAsTextMessage(m *message, body *util.PeekedReadCloser) error {
	if !utf8.Valid(body.PeekedBytes) {
		return errHTTPBadRequestMessageNotUTF8
	}
	if len(body.PeekedBytes) > 0 { // Empty body should not override message (publish via GET!)
		m.Message = strings.TrimSpace(string(body.PeekedBytes)) // Truncates the message to the peek limit if required
	}
	if m.Attachment != nil && m.Attachment.Name != "" && m.Message == "" {
		m.Message = fmt.Sprintf(defaultAttachmentMessage, m.Attachment.Name)
	}
	return nil
}

func (s *Server) handleBodyAsAttachment(r *http.Request, v *visitor, m *message, body *util.PeekedReadCloser) error {
	if s.fileCache == nil || s.config.BaseURL == "" || s.config.AttachmentCacheDir == "" {
		return errHTTPBadRequestAttachmentsDisallowed
	} else if m.Time > time.Now().Add(s.config.AttachmentExpiryDuration).Unix() {
		return errHTTPBadRequestAttachmentsExpiryBeforeDelivery
	}
	visitorStats, err := v.Stats()
	if err != nil {
		return err
	}
	contentLengthStr := r.Header.Get("Content-Length")
	if contentLengthStr != "" { // Early "do-not-trust" check, hard limit see below
		contentLength, err := strconv.ParseInt(contentLengthStr, 10, 64)
		if err == nil && (contentLength > visitorStats.VisitorAttachmentBytesRemaining || contentLength > s.config.AttachmentFileSizeLimit) {
			return errHTTPEntityTooLargeAttachmentTooLarge
		}
	}
	if m.Attachment == nil {
		m.Attachment = &attachment{}
	}
	var ext string
	m.Attachment.Owner = v.ip // Important for attachment rate limiting
	m.Attachment.Expires = time.Now().Add(s.config.AttachmentExpiryDuration).Unix()
	m.Attachment.Type, ext = util.DetectContentType(body.PeekedBytes, m.Attachment.Name)
	m.Attachment.URL = fmt.Sprintf("%s/file/%s%s", s.config.BaseURL, m.ID, ext)
	if m.Attachment.Name == "" {
		m.Attachment.Name = fmt.Sprintf("attachment%s", ext)
	}
	if m.Message == "" {
		m.Message = fmt.Sprintf(defaultAttachmentMessage, m.Attachment.Name)
	}
	m.Attachment.Size, err = s.fileCache.Write(m.ID, body, v.BandwidthLimiter(), util.NewFixedLimiter(visitorStats.VisitorAttachmentBytesRemaining))
	if err == util.ErrLimitReached {
		return errHTTPEntityTooLargeAttachmentTooLarge
	} else if err != nil {
		return err
	}
	return nil
}

func (s *Server) handleSubscribeJSON(w http.ResponseWriter, r *http.Request, v *visitor) error {
	encoder := func(msg *message) (string, error) {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(&msg); err != nil {
			return "", err
		}
		return buf.String(), nil
	}
	return s.handleSubscribeHTTP(w, r, v, "application/x-ndjson", encoder)
}

func (s *Server) handleSubscribeSSE(w http.ResponseWriter, r *http.Request, v *visitor) error {
	encoder := func(msg *message) (string, error) {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(&msg); err != nil {
			return "", err
		}
		if msg.Event != messageEvent {
			return fmt.Sprintf("event: %s\ndata: %s\n", msg.Event, buf.String()), nil // Browser's .onmessage() does not fire on this!
		}
		return fmt.Sprintf("data: %s\n", buf.String()), nil
	}
	return s.handleSubscribeHTTP(w, r, v, "text/event-stream", encoder)
}

func (s *Server) handleSubscribeRaw(w http.ResponseWriter, r *http.Request, v *visitor) error {
	encoder := func(msg *message) (string, error) {
		if msg.Event == messageEvent { // only handle default events
			return strings.ReplaceAll(msg.Message, "\n", " ") + "\n", nil
		}
		return "\n", nil // "keepalive" and "open" events just send an empty line
	}
	return s.handleSubscribeHTTP(w, r, v, "text/plain", encoder)
}

func (s *Server) handleSubscribeHTTP(w http.ResponseWriter, r *http.Request, v *visitor, contentType string, encoder messageEncoder) error {
	if err := v.SubscriptionAllowed(); err != nil {
		return errHTTPTooManyRequestsLimitSubscriptions
	}
	defer v.RemoveSubscription()
	topics, topicsStr, err := s.topicsFromPath(r.URL.Path)
	if err != nil {
		return err
	}
	poll, since, scheduled, filters, err := parseSubscribeParams(r)
	if err != nil {
		return err
	}
	var wlock sync.Mutex
	sub := func(msg *message) error {
		if !filters.Pass(msg) {
			return nil
		}
		m, err := encoder(msg)
		if err != nil {
			return err
		}
		wlock.Lock()
		defer wlock.Unlock()
		if _, err := w.Write([]byte(m)); err != nil {
			return err
		}
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		return nil
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")            // CORS, allow cross-origin requests
	w.Header().Set("Content-Type", contentType+"; charset=utf-8") // Android/Volley client needs charset!
	if poll {
		return s.sendOldMessages(topics, since, scheduled, sub)
	}
	subscriberIDs := make([]int, 0)
	for _, t := range topics {
		subscriberIDs = append(subscriberIDs, t.Subscribe(sub))
	}
	defer func() {
		for i, subscriberID := range subscriberIDs {
			topics[i].Unsubscribe(subscriberID) // Order!
		}
	}()
	if err := sub(newOpenMessage(topicsStr)); err != nil { // Send out open message
		return err
	}
	if err := s.sendOldMessages(topics, since, scheduled, sub); err != nil {
		return err
	}
	for {
		select {
		case <-r.Context().Done():
			return nil
		case <-time.After(s.config.KeepaliveInterval):
			v.Keepalive()
			if err := sub(newKeepaliveMessage(topicsStr)); err != nil { // Send keepalive message
				return err
			}
		}
	}
}

func (s *Server) handleSubscribeWS(w http.ResponseWriter, r *http.Request, v *visitor) error {
	if strings.ToLower(r.Header.Get("Upgrade")) != "websocket" {
		return errHTTPBadRequestWebSocketsUpgradeHeaderMissing
	}
	if err := v.SubscriptionAllowed(); err != nil {
		return errHTTPTooManyRequestsLimitSubscriptions
	}
	defer v.RemoveSubscription()
	topics, topicsStr, err := s.topicsFromPath(r.URL.Path)
	if err != nil {
		return err
	}
	poll, since, scheduled, filters, err := parseSubscribeParams(r)
	if err != nil {
		return err
	}
	upgrader := &websocket.Upgrader{
		ReadBufferSize:  wsBufferSize,
		WriteBufferSize: wsBufferSize,
		CheckOrigin: func(r *http.Request) bool {
			return true // We're open for business!
		},
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	var wlock sync.Mutex
	g, ctx := errgroup.WithContext(context.Background())
	g.Go(func() error {
		pongWait := s.config.KeepaliveInterval + wsPongWait
		conn.SetReadLimit(wsReadLimit)
		if err := conn.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
			return err
		}
		conn.SetPongHandler(func(appData string) error {
			return conn.SetReadDeadline(time.Now().Add(pongWait))
		})
		for {
			_, _, err := conn.NextReader()
			if err != nil {
				return err
			}
		}
	})
	g.Go(func() error {
		ping := func() error {
			wlock.Lock()
			defer wlock.Unlock()
			if err := conn.SetWriteDeadline(time.Now().Add(wsWriteWait)); err != nil {
				return err
			}
			return conn.WriteMessage(websocket.PingMessage, nil)
		}
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(s.config.KeepaliveInterval):
				v.Keepalive()
				if err := ping(); err != nil {
					return err
				}
			}
		}
	})
	sub := func(msg *message) error {
		if !filters.Pass(msg) {
			return nil
		}
		wlock.Lock()
		defer wlock.Unlock()
		if err := conn.SetWriteDeadline(time.Now().Add(wsWriteWait)); err != nil {
			return err
		}
		return conn.WriteJSON(msg)
	}
	w.Header().Set("Access-Control-Allow-Origin", "*") // CORS, allow cross-origin requests
	if poll {
		return s.sendOldMessages(topics, since, scheduled, sub)
	}
	subscriberIDs := make([]int, 0)
	for _, t := range topics {
		subscriberIDs = append(subscriberIDs, t.Subscribe(sub))
	}
	defer func() {
		for i, subscriberID := range subscriberIDs {
			topics[i].Unsubscribe(subscriberID) // Order!
		}
	}()
	if err := sub(newOpenMessage(topicsStr)); err != nil { // Send out open message
		return err
	}
	if err := s.sendOldMessages(topics, since, scheduled, sub); err != nil {
		return err
	}
	err = g.Wait()
	if err != nil && websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
		return nil // Normal closures are not errors
	}
	return err
}

func parseSubscribeParams(r *http.Request) (poll bool, since sinceMarker, scheduled bool, filters *queryFilter, err error) {
	poll = readBoolParam(r, false, "x-poll", "poll", "po")
	scheduled = readBoolParam(r, false, "x-scheduled", "scheduled", "sched")
	since, err = parseSince(r, poll)
	if err != nil {
		return
	}
	filters, err = parseQueryFilters(r)
	if err != nil {
		return
	}
	return
}

func (s *Server) sendOldMessages(topics []*topic, since sinceMarker, scheduled bool, sub subscriber) error {
	if since.IsNone() {
		return nil
	}
	for _, t := range topics {
		messages, err := s.messageCache.Messages(t.ID, since, scheduled)
		if err != nil {
			return err
		}
		for _, m := range messages {
			if err := sub(m); err != nil {
				return err
			}
		}
	}
	return nil
}

// parseSince returns a timestamp identifying the time span from which cached messages should be received.
//
// Values in the "since=..." parameter can be either a unix timestamp or a duration (e.g. 12h), or
// "all" for all messages.
func parseSince(r *http.Request, poll bool) (sinceMarker, error) {
	since := readParam(r, "x-since", "since", "si")

	// Easy cases (empty, all, none)
	if since == "" {
		if poll {
			return sinceAllMessages, nil
		}
		return sinceNoMessages, nil
	} else if since == "all" {
		return sinceAllMessages, nil
	} else if since == "none" {
		return sinceNoMessages, nil
	}

	// ID, timestamp, duration
	if validMessageID(since) {
		return newSinceID(since), nil
	} else if s, err := strconv.ParseInt(since, 10, 64); err == nil {
		return newSinceTime(s), nil
	} else if d, err := time.ParseDuration(since); err == nil {
		return newSinceTime(time.Now().Add(-1 * d).Unix()), nil
	}
	return sinceNoMessages, errHTTPBadRequestSinceInvalid
}

func (s *Server) handleOptions(w http.ResponseWriter, _ *http.Request) error {
	w.Header().Set("Access-Control-Allow-Methods", "GET, PUT, POST")
	w.Header().Set("Access-Control-Allow-Origin", "*")  // CORS, allow cross-origin requests
	w.Header().Set("Access-Control-Allow-Headers", "*") // CORS, allow auth via JS // FIXME is this terrible?
	return nil
}

func (s *Server) topicFromPath(path string) (*topic, error) {
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return nil, errHTTPBadRequestTopicInvalid
	}
	topics, err := s.topicsFromIDs(parts[1])
	if err != nil {
		return nil, err
	}
	return topics[0], nil
}

func (s *Server) topicsFromPath(path string) ([]*topic, string, error) {
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return nil, "", errHTTPBadRequestTopicInvalid
	}
	topicIDs := util.SplitNoEmpty(parts[1], ",")
	topics, err := s.topicsFromIDs(topicIDs...)
	if err != nil {
		return nil, "", errHTTPBadRequestTopicInvalid
	}
	return topics, parts[1], nil
}

func (s *Server) topicsFromIDs(ids ...string) ([]*topic, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	topics := make([]*topic, 0)
	for _, id := range ids {
		if util.InStringList(disallowedTopics, id) {
			return nil, errHTTPBadRequestTopicDisallowed
		}
		if _, ok := s.topics[id]; !ok {
			if len(s.topics) >= s.config.TotalTopicLimit {
				return nil, errHTTPTooManyRequestsLimitTotalTopics
			}
			s.topics[id] = newTopic(id)
		}
		topics = append(topics, s.topics[id])
	}
	return topics, nil
}

func (s *Server) updateStatsAndPrune() {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Expire visitors from rate visitors map
	for ip, v := range s.visitors {
		if v.Stale() {
			delete(s.visitors, ip)
		}
	}

	// Delete expired attachments
	if s.fileCache != nil {
		ids, err := s.messageCache.AttachmentsExpired()
		if err == nil {
			if err := s.fileCache.Remove(ids...); err != nil {
				log.Printf("error while deleting attachments: %s", err.Error())
			}
		} else {
			log.Printf("error retrieving expired attachments: %s", err.Error())
		}
	}

	// Prune message cache
	olderThan := time.Now().Add(-1 * s.config.CacheDuration)
	if err := s.messageCache.Prune(olderThan); err != nil {
		log.Printf("error pruning cache: %s", err.Error())
	}

	// Prune old topics, remove subscriptions without subscribers
	var subscribers, messages int
	for _, t := range s.topics {
		subs := t.Subscribers()
		msgs, err := s.messageCache.MessageCount(t.ID)
		if err != nil {
			log.Printf("cannot get stats for topic %s: %s", t.ID, err.Error())
			continue
		}
		if msgs == 0 && subs == 0 {
			delete(s.topics, t.ID)
			continue
		}
		subscribers += subs
		messages += msgs
	}

	// Mail stats
	var mailSuccess, mailFailure int64
	if s.smtpBackend != nil {
		mailSuccess, mailFailure = s.smtpBackend.Counts()
	}

	// Print stats
	log.Printf("Stats: %d message(s) published, %d in cache, %d successful mails, %d failed, %d topic(s) active, %d subscriber(s), %d visitor(s)",
		s.messages, messages, mailSuccess, mailFailure, len(s.topics), subscribers, len(s.visitors))
}

func (s *Server) runSMTPServer() error {
	sub := func(m *message) error {
		url := fmt.Sprintf("%s/%s", s.config.BaseURL, m.Topic)
		req, err := http.NewRequest("PUT", url, strings.NewReader(m.Message))
		if err != nil {
			return err
		}
		if m.Title != "" {
			req.Header.Set("Title", m.Title)
		}
		rr := httptest.NewRecorder()
		s.handle(rr, req)
		if rr.Code != http.StatusOK {
			return errors.New("error: " + rr.Body.String())
		}
		return nil
	}
	s.smtpBackend = newMailBackend(s.config, sub)
	s.smtpServer = smtp.NewServer(s.smtpBackend)
	s.smtpServer.Addr = s.config.SMTPServerListen
	s.smtpServer.Domain = s.config.SMTPServerDomain
	s.smtpServer.ReadTimeout = 10 * time.Second
	s.smtpServer.WriteTimeout = 10 * time.Second
	s.smtpServer.MaxMessageBytes = 1024 * 1024 // Must be much larger than message size (headers, multipart, etc.)
	s.smtpServer.MaxRecipients = 1
	s.smtpServer.AllowInsecureAuth = true
	return s.smtpServer.ListenAndServe()
}

func (s *Server) runManager() {
	for {
		select {
		case <-time.After(s.config.ManagerInterval):
			s.updateStatsAndPrune()
		case <-s.closeChan:
			return
		}
	}
}

func (s *Server) runAtSender() {
	for {
		select {
		case <-time.After(s.config.AtSenderInterval):
			if err := s.sendDelayedMessages(); err != nil {
				log.Printf("error sending scheduled messages: %s", err.Error())
			}
		case <-s.closeChan:
			return
		}
	}
}

func (s *Server) runFirebaseKeepaliver() {
	if s.firebase == nil {
		return
	}
	for {
		select {
		case <-time.After(s.config.FirebaseKeepaliveInterval):
			if err := s.firebase(newKeepaliveMessage(firebaseControlTopic)); err != nil {
				log.Printf("error sending Firebase keepalive message: %s", err.Error())
			}
		case <-s.closeChan:
			return
		}
	}
}

func (s *Server) sendDelayedMessages() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	messages, err := s.messageCache.MessagesDue()
	if err != nil {
		return err
	}
	for _, m := range messages {
		t, ok := s.topics[m.Topic] // If no subscribers, just mark message as published
		if ok {
			if err := t.Publish(m); err != nil {
				log.Printf("unable to publish message %s to topic %s: %v", m.ID, m.Topic, err.Error())
			}
		}
		if s.firebase != nil { // Firebase subscribers may not show up in topics map
			if err := s.firebase(m); err != nil {
				log.Printf("unable to publish to Firebase: %v", err.Error())
			}
		}
		if err := s.messageCache.MarkPublished(m); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) limitRequests(next handleFunc) handleFunc {
	return func(w http.ResponseWriter, r *http.Request, v *visitor) error {
		if util.InStringList(s.config.VisitorRequestExemptIPAddrs, v.ip) {
			return next(w, r, v)
		} else if err := v.RequestAllowed(); err != nil {
			return errHTTPTooManyRequestsLimitRequests
		}
		return next(w, r, v)
	}
}

// transformBodyJSON peeks the request body, reads the JSON, and converts it to headers
// before passing it on to the next handler. This is meant to be used in combination with handlePublish.
func (s *Server) transformBodyJSON(next handleFunc) handleFunc {
	return func(w http.ResponseWriter, r *http.Request, v *visitor) error {
		body, err := util.Peek(r.Body, s.config.MessageLimit)
		if err != nil {
			return err
		}
		defer r.Body.Close()
		var m publishMessage
		if err := json.NewDecoder(body).Decode(&m); err != nil {
			return errHTTPBadRequestJSONInvalid
		}
		if !topicRegex.MatchString(m.Topic) {
			return errHTTPBadRequestTopicInvalid
		}
		if m.Message == "" {
			m.Message = emptyMessageBody
		}
		r.URL.Path = "/" + m.Topic
		r.Body = io.NopCloser(strings.NewReader(m.Message))
		if m.Title != "" {
			r.Header.Set("X-Title", m.Title)
		}
		if m.Priority != 0 {
			r.Header.Set("X-Priority", fmt.Sprintf("%d", m.Priority))
		}
		if m.Tags != nil && len(m.Tags) > 0 {
			r.Header.Set("X-Tags", strings.Join(m.Tags, ","))
		}
		if m.Attach != "" {
			r.Header.Set("X-Attach", m.Attach)
		}
		if m.Filename != "" {
			r.Header.Set("X-Filename", m.Filename)
		}
		if m.Click != "" {
			r.Header.Set("X-Click", m.Click)
		}
		if len(m.Actions) > 0 {
			actionsStr, err := json.Marshal(m.Actions)
			if err != nil {
				return errHTTPBadRequestJSONInvalid
			}
			r.Header.Set("X-Actions", string(actionsStr))
		}
		if m.Email != "" {
			r.Header.Set("X-Email", m.Email)
		}
		if m.Delay != "" {
			r.Header.Set("X-Delay", m.Delay)
		}
		return next(w, r, v)
	}
}

func (s *Server) authWrite(next handleFunc) handleFunc {
	return s.withAuth(next, auth.PermissionWrite)
}

func (s *Server) authRead(next handleFunc) handleFunc {
	return s.withAuth(next, auth.PermissionRead)
}

func (s *Server) withAuth(next handleFunc, perm auth.Permission) handleFunc {
	return func(w http.ResponseWriter, r *http.Request, v *visitor) error {
		if s.auth == nil {
			return next(w, r, v)
		}
		topics, _, err := s.topicsFromPath(r.URL.Path)
		if err != nil {
			return err
		}
		var user *auth.User // may stay nil if no auth header!
		username, password, ok := extractUserPass(r)
		if ok {
			if user, err = s.auth.Authenticate(username, password); err != nil {
				log.Printf("authentication failed: %s", err.Error())
				return errHTTPUnauthorized
			}
		}
		for _, t := range topics {
			if err := s.auth.Authorize(user, t.ID, perm); err != nil {
				log.Printf("unauthorized: %s", err.Error())
				return errHTTPForbidden
			}
		}
		return next(w, r, v)
	}
}

// extractUserPass reads the username/password from the basic auth header (Authorization: Basic ...),
// or from the ?auth=... query param. The latter is required only to support the WebSocket JavaScript
// class, which does not support passing headers during the initial request. The auth query param
// is effectively double base64 encoded. Its format is base64(Basic base64(user:pass)).
func extractUserPass(r *http.Request) (username string, password string, ok bool) {
	username, password, ok = r.BasicAuth()
	if ok {
		return
	}
	authParam := readQueryParam(r, "authorization", "auth")
	if authParam != "" {
		a, err := base64.RawURLEncoding.DecodeString(authParam)
		if err != nil {
			return
		}
		r.Header.Set("Authorization", string(a))
		return r.BasicAuth()
	}
	return
}

// visitor creates or retrieves a rate.Limiter for the given visitor.
// This function was taken from https://www.alexedwards.net/blog/how-to-rate-limit-http-requests (MIT).
func (s *Server) visitor(r *http.Request) *visitor {
	s.mu.Lock()
	defer s.mu.Unlock()
	remoteAddr := r.RemoteAddr
	ip, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		ip = remoteAddr // This should not happen in real life; only in tests.
	}
	if s.config.BehindProxy && r.Header.Get("X-Forwarded-For") != "" {
		ip = r.Header.Get("X-Forwarded-For")
	}
	v, exists := s.visitors[ip]
	if !exists {
		s.visitors[ip] = newVisitor(s.config, s.messageCache, ip)
		return s.visitors[ip]
	}
	v.Keepalive()
	return v
}
