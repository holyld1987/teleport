/*
Copyright 2015-2020 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package events

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"

	"github.com/gravitational/teleport"
	apidefaults "github.com/gravitational/teleport/api/defaults"
	"github.com/gravitational/teleport/api/types"
	apievents "github.com/gravitational/teleport/api/types/events"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/observability/metrics"
	"github.com/gravitational/teleport/lib/session"
	"github.com/gravitational/teleport/lib/utils"
)

const (
	// SessionLogsDir is a subdirectory inside the eventlog data dir
	// where all session-specific logs and streams are stored, like
	// in /var/lib/teleport/log/sessions
	SessionLogsDir = "sessions"

	// StreamingLogsDir is a subdirectory of sessions /var/lib/teleport/log/streaming
	// is used in new versions of the uploader. This directory is used in asynchronous
	// recording modes where recordings are buffered to disk before being uploaded
	// to the auth server.
	StreamingLogsDir = "streaming"

	// RecordsDir is an auth server subdirectory with session recordings that is used
	// when the auth server is not configured for external cloud storage. It is not
	// used by nodes, proxies, or other Teleport services.
	RecordsDir = "records"

	// PlaybackDir is a directory for caching downloaded sessions during playback.
	PlaybackDir = "playbacks"

	// LogfileExt defines the ending of the daily event log file
	LogfileExt = ".log"

	// SymlinkFilename is a name of the symlink pointing to the last
	// current log file
	SymlinkFilename = "events.log"
)

var (
	auditOpenFiles = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "audit_server_open_files",
			Help: "Number of open audit files",
		},
	)

	auditDiskUsed = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "audit_percentage_disk_space_used",
			Help: "Percentage disk space used.",
		},
	)

	auditFailedDisk = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "audit_failed_disk_monitoring",
			Help: "Number of times disk monitoring failed.",
		},
	)
	// AuditFailedEmit increments the counter if audit event failed to emit
	AuditFailedEmit = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "audit_failed_emit_events",
			Help: "Number of times emitting audit event failed.",
		},
	)

	auditEmitEvent = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: teleport.MetricNamespace,
			Name:      "audit_emit_events",
			Help:      "Number of audit events emitted",
		},
	)

	prometheusCollectors = []prometheus.Collector{auditOpenFiles, auditDiskUsed, auditFailedDisk, AuditFailedEmit, auditEmitEvent}
)

// AuditLog is a new combined facility to record Teleport events and
// sessions. It implements IAuditLog
type AuditLog struct {
	sync.RWMutex
	AuditLogConfig

	// log specifies the logger
	log log.FieldLogger

	// playbackDir is a directory used for unpacked session recordings
	playbackDir string

	// activeDownloads helps to serialize simultaneous downloads
	// from the session record server
	activeDownloads map[string]context.Context

	// ctx signals close of the audit log
	ctx context.Context

	// cancel triggers closing of the signal context
	cancel context.CancelFunc

	// localLog is a local events log used
	// to emit audit events if no external log has been specified
	localLog *FileLog
}

// AuditLogConfig specifies configuration for AuditLog server
type AuditLogConfig struct {
	// DataDir is the directory where audit log stores the data
	DataDir string

	// ServerID is the id of the audit log server
	ServerID string

	// RotationPeriod defines how frequently to rotate the log file
	RotationPeriod time.Duration

	// Clock is a clock either real one or used in tests
	Clock clockwork.Clock

	// UIDGenerator is used to generate unique IDs for events
	UIDGenerator utils.UID

	// GID if provided will be used to set group ownership of the directory
	// to GID
	GID *int

	// UID if provided will be used to set user ownership of the directory
	// to UID
	UID *int

	// DirMask if provided will be used to set directory mask access
	// otherwise set to default value
	DirMask *os.FileMode

	// PlaybackRecycleTTL is a time after uncompressed playback files will be
	// deleted
	PlaybackRecycleTTL time.Duration

	// UploadHandler is a pluggable external upload handler,
	// used to fetch sessions from external sources
	UploadHandler MultipartHandler

	// ExternalLog is a pluggable external log service
	ExternalLog IAuditLog

	// Context is audit log context
	Context context.Context
}

// CheckAndSetDefaults checks and sets defaults
func (a *AuditLogConfig) CheckAndSetDefaults() error {
	if a.DataDir == "" {
		return trace.BadParameter("missing parameter DataDir")
	}
	if a.ServerID == "" {
		return trace.BadParameter("missing parameter ServerID")
	}
	if a.UploadHandler == nil {
		return trace.BadParameter("missing parameter UploadHandler")
	}
	if a.Clock == nil {
		a.Clock = clockwork.NewRealClock()
	}
	if a.UIDGenerator == nil {
		a.UIDGenerator = utils.NewRealUID()
	}
	if a.RotationPeriod == 0 {
		a.RotationPeriod = defaults.LogRotationPeriod
	}
	if a.DirMask == nil {
		mask := os.FileMode(teleport.DirMaskSharedGroup)
		a.DirMask = &mask
	}
	if (a.GID != nil && a.UID == nil) || (a.UID != nil && a.GID == nil) {
		return trace.BadParameter("if UID or GID is set, both should be specified")
	}
	if a.PlaybackRecycleTTL == 0 {
		a.PlaybackRecycleTTL = defaults.PlaybackRecycleTTL
	}
	if a.Context == nil {
		a.Context = context.Background()
	}
	return nil
}

// NewAuditLog creates and returns a new Audit Log object which will store its log files in
// a given directory.
func NewAuditLog(cfg AuditLogConfig) (*AuditLog, error) {
	err := metrics.RegisterPrometheusCollectors(prometheusCollectors...)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if err := cfg.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}
	ctx, cancel := context.WithCancel(cfg.Context)
	al := &AuditLog{
		playbackDir:    filepath.Join(cfg.DataDir, PlaybackDir, SessionLogsDir, apidefaults.Namespace),
		AuditLogConfig: cfg,
		log: log.WithFields(log.Fields{
			trace.Component: teleport.ComponentAuditLog,
		}),
		activeDownloads: make(map[string]context.Context),
		ctx:             ctx,
		cancel:          cancel,
	}
	// create a directory for audit logs, audit log does not create
	// session logs before migrations are run in case if the directory
	// has to be moved
	auditDir := filepath.Join(cfg.DataDir, cfg.ServerID)
	if err := os.MkdirAll(auditDir, *cfg.DirMask); err != nil {
		return nil, trace.ConvertSystemError(err)
	}
	// create a directory for session logs:
	sessionDir := filepath.Join(cfg.DataDir, cfg.ServerID, SessionLogsDir, apidefaults.Namespace)
	if err := os.MkdirAll(sessionDir, *cfg.DirMask); err != nil {
		return nil, trace.ConvertSystemError(err)
	}
	// create a directory for uncompressed playbacks
	if err := os.MkdirAll(filepath.Join(al.playbackDir), *cfg.DirMask); err != nil {
		return nil, trace.ConvertSystemError(err)
	}
	if cfg.UID != nil && cfg.GID != nil {
		err := os.Chown(cfg.DataDir, *cfg.UID, *cfg.GID)
		if err != nil {
			return nil, trace.ConvertSystemError(err)
		}
		err = os.Chown(sessionDir, *cfg.UID, *cfg.GID)
		if err != nil {
			return nil, trace.ConvertSystemError(err)
		}
		err = os.Chown(al.playbackDir, *cfg.UID, *cfg.GID)
		if err != nil {
			return nil, trace.ConvertSystemError(err)
		}
	}

	if al.ExternalLog == nil {
		var err error
		al.localLog, err = NewFileLog(FileLogConfig{
			RotationPeriod: al.RotationPeriod,
			Dir:            auditDir,
			SymlinkDir:     cfg.DataDir,
			Clock:          al.Clock,
			UIDGenerator:   al.UIDGenerator,
			SearchDirs:     al.auditDirs,
		})
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}

	go al.periodicCleanupPlaybacks()
	go al.periodicSpaceMonitor()

	return al, nil
}

func getAuthServers(dataDir string) ([]string, error) {
	// scan the log directory:
	df, err := os.Open(dataDir)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer df.Close()
	entries, err := df.Readdir(-1)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	var authServers []string
	for i := range entries {
		fi := entries[i]
		if fi.IsDir() {
			fileName := filepath.Base(fi.Name())
			// TODO: this is not the best solution because these names
			// can be colliding with customer-picked names, so consider
			// moving the folders to a folder level up and keep the servers
			// one small
			if fileName != PlaybackDir && fileName != teleport.ComponentUpload && fileName != RecordsDir {
				authServers = append(authServers, fileName)
			}
		}
	}
	return authServers, nil
}

type sessionIndex struct {
	dataDir        string
	namespace      string
	sid            session.ID
	events         []indexEntry
	enhancedEvents map[string][]indexEntry
	chunks         []indexEntry
	indexFiles     []string
}

func (idx *sessionIndex) sort() {
	sort.Slice(idx.events, func(i, j int) bool {
		return idx.events[i].Index < idx.events[j].Index
	})
	sort.Slice(idx.chunks, func(i, j int) bool {
		return idx.chunks[i].Offset < idx.chunks[j].Offset
	})

	// Enhanced events.
	for _, events := range idx.enhancedEvents {
		sort.Slice(events, func(i, j int) bool {
			return events[i].Index < events[j].Index
		})
	}
}

func (idx *sessionIndex) eventsFileName(index int) string {
	entry := idx.events[index]
	return filepath.Join(idx.dataDir, entry.authServer, SessionLogsDir, idx.namespace, entry.FileName)
}

func (idx *sessionIndex) eventsFile(afterN int) (int, error) {
	for i := len(idx.events) - 1; i >= 0; i-- {
		entry := idx.events[i]
		if int64(afterN) >= entry.Index {
			return i, nil
		}
	}
	return -1, trace.NotFound("%v not found", afterN)
}

// chunkFileNames returns file names of all session chunk files
func (idx *sessionIndex) chunkFileNames() []string {
	fileNames := make([]string, len(idx.chunks))
	for i := 0; i < len(idx.chunks); i++ {
		fileNames[i] = idx.chunksFileName(i)
	}
	return fileNames
}

func (idx *sessionIndex) chunksFile(offset int64) (string, int64, error) {
	for i := len(idx.chunks) - 1; i >= 0; i-- {
		entry := idx.chunks[i]
		if offset >= entry.Offset {
			return idx.chunksFileName(i), entry.Offset, nil
		}
	}
	return "", 0, trace.NotFound("offset %v not found for session %v", offset, idx.sid)
}

func (idx *sessionIndex) chunksFileName(index int) string {
	entry := idx.chunks[index]
	return filepath.Join(idx.dataDir, entry.authServer, SessionLogsDir, idx.namespace, entry.FileName)
}

func (l *AuditLog) readSessionIndex(namespace string, sid session.ID) (*sessionIndex, error) {
	index, err := readSessionIndex(l.DataDir, []string{PlaybackDir}, namespace, sid)
	if err == nil {
		return index, nil
	}
	if !trace.IsNotFound(err) {
		return nil, trace.Wrap(err)
	}
	// some legacy records may be stored unpacked in the JSON format
	// in the data dir, under server format
	authServers, err := getAuthServers(l.DataDir)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return readSessionIndex(l.DataDir, authServers, namespace, sid)
}

func readSessionIndex(dataDir string, authServers []string, namespace string, sid session.ID) (*sessionIndex, error) {
	index := sessionIndex{
		sid:       sid,
		dataDir:   dataDir,
		namespace: namespace,
		enhancedEvents: map[string][]indexEntry{
			SessionCommandEvent: {},
			SessionDiskEvent:    {},
			SessionNetworkEvent: {},
		},
	}
	for _, authServer := range authServers {
		indexFileName := filepath.Join(dataDir, authServer, SessionLogsDir, namespace, fmt.Sprintf("%v.index", sid))
		indexFile, err := os.OpenFile(indexFileName, os.O_RDONLY, 0640)
		err = trace.ConvertSystemError(err)
		if err != nil {
			if !trace.IsNotFound(err) {
				return nil, trace.Wrap(err)
			}
			continue
		}
		index.indexFiles = append(index.indexFiles, indexFileName)

		entries, err := readIndexEntries(indexFile, authServer)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		for _, entry := range entries {
			switch entry.Type {
			case fileTypeEvents:
				index.events = append(index.events, entry)
			case fileTypeChunks:
				index.chunks = append(index.chunks, entry)
			// Enhanced events.
			case SessionCommandEvent, SessionDiskEvent, SessionNetworkEvent:
				index.enhancedEvents[entry.Type] = append(index.enhancedEvents[entry.Type], entry)
			default:
				return nil, trace.BadParameter("found unknown event type: %q", entry.Type)
			}
		}

		err = indexFile.Close()
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}

	if len(index.indexFiles) == 0 {
		return nil, trace.NotFound("session %q not found", sid)
	}

	index.sort()
	return &index, nil
}

func readIndexEntries(file *os.File, authServer string) ([]indexEntry, error) {
	var entries []indexEntry

	scanner := bufio.NewScanner(file)
	for lineNo := 0; scanner.Scan(); lineNo++ {
		var entry indexEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return nil, trace.Wrap(err)
		}
		entry.authServer = authServer
		entries = append(entries, entry)
	}

	return entries, nil
}

// createOrGetDownload creates a new download sync entry for a given session,
// if there is no active download in progress, or returns an existing one.
// if the new context has been created, cancel function is returned as a
// second argument. Caller should call this function to signal that download has been
// completed or failed.
func (l *AuditLog) createOrGetDownload(path string) (context.Context, context.CancelFunc) {
	l.Lock()
	defer l.Unlock()
	ctx, ok := l.activeDownloads[path]
	if ok {
		return ctx, nil
	}
	ctx, cancel := context.WithCancel(context.TODO())
	l.activeDownloads[path] = ctx
	return ctx, func() {
		cancel()
		l.Lock()
		defer l.Unlock()
		delete(l.activeDownloads, path)
	}
}

func (l *AuditLog) downloadSession(namespace string, sid session.ID) error {
	tarballPath := filepath.Join(l.playbackDir, string(sid)+".tar")

	ctx, cancel := l.createOrGetDownload(tarballPath)
	// means that another download is in progress, so simply wait until
	// it finishes
	if cancel == nil {
		l.log.Debugf("Another download is in progress for %v, waiting until it gets completed.", sid)
		select {
		case <-ctx.Done():
			return nil
		case <-l.ctx.Done():
			return trace.BadParameter("audit log is closing, aborting the download")
		}
	}
	defer cancel()
	_, err := os.Stat(tarballPath)
	err = trace.ConvertSystemError(err)
	if err == nil {
		l.log.Debugf("Recording %v is already downloaded and unpacked to %v.", sid, tarballPath)
		return nil
	}
	if !trace.IsNotFound(err) {
		return trace.Wrap(err)
	}
	start := time.Now()
	l.log.Debugf("Starting download of %v.", sid)
	tarball, err := os.OpenFile(tarballPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0640)
	if err != nil {
		return trace.ConvertSystemError(err)
	}
	defer func() {
		if err := tarball.Close(); err != nil {
			l.log.WithError(err).Errorf("Failed to close file %q.", tarballPath)
		}
	}()
	if err := l.UploadHandler.Download(l.ctx, sid, tarball); err != nil {
		// remove partially downloaded tarball
		if rmErr := os.Remove(tarballPath); rmErr != nil {
			l.log.WithError(rmErr).Warningf("Failed to remove file %v.", tarballPath)
		}
		return trace.Wrap(err)
	}
	l.log.WithField("duration", time.Since(start)).Debugf("Downloaded %v to %v.", sid, tarballPath)

	_, err = tarball.Seek(0, 0)
	if err != nil {
		return trace.ConvertSystemError(err)
	}
	format, err := DetectFormat(tarball)
	if err != nil {
		l.log.WithError(err).Debugf("Failed to detect playback %v format.", tarballPath)
		return trace.Wrap(err)
	}
	_, err = tarball.Seek(0, 0)
	if err != nil {
		return trace.ConvertSystemError(err)
	}
	switch {
	case format.Proto:
		start = time.Now()
		l.log.Debugf("Converting %v to playback format.", tarballPath)
		protoReader := NewProtoReader(tarball)
		_, err = WriteForSSHPlayback(l.Context, sid, protoReader, l.playbackDir)
		if err != nil {
			l.log.WithError(err).Error("Failed to convert.")
			return trace.Wrap(err)
		}
		stats := protoReader.GetStats().ToFields()
		stats["duration"] = time.Since(start)
		l.log.WithFields(stats).Debugf("Converted %v to %v.", tarballPath, l.playbackDir)
	case format.Tar:
		if err := utils.Extract(tarball, l.playbackDir); err != nil {
			return trace.Wrap(err)
		}
	default:
		return trace.BadParameter("Unexpected format %v.", format)
	}

	// Extract every chunks file on disk while holding the context,
	// otherwise parallel downloads will try to unpack the file at the same time.
	idx, err := l.readSessionIndex(namespace, sid)
	if err != nil {
		return trace.Wrap(err)
	}
	for _, fileName := range idx.chunkFileNames() {
		reader, err := l.unpackFile(fileName)
		if err != nil {
			return trace.Wrap(err)
		}
		if err := reader.Close(); err != nil {
			l.log.Warningf("Failed to close file: %v.", err)
		}
	}
	l.log.WithField("duration", time.Since(start)).Debugf("Unpacked %v to %v.", tarballPath, l.playbackDir)
	return nil
}

// GetSessionChunk returns a reader which console and web clients request
// to receive a live stream of a given session. The reader allows access to a
// session stream range from offsetBytes to offsetBytes+maxBytes
func (l *AuditLog) GetSessionChunk(namespace string, sid session.ID, offsetBytes, maxBytes int) ([]byte, error) {
	if err := l.downloadSession(namespace, sid); err != nil {
		return nil, trace.Wrap(err)
	}
	var data []byte
	for {
		out, err := l.getSessionChunk(namespace, sid, offsetBytes, maxBytes)
		if err != nil {
			if err == io.EOF {
				return data, nil
			}
			return nil, trace.Wrap(err)
		}
		data = append(data, out...)
		if len(data) == maxBytes || len(out) == 0 {
			return data, nil
		}
		maxBytes = maxBytes - len(out)
		offsetBytes = offsetBytes + len(out)
	}
}

func (l *AuditLog) cleanupOldPlaybacks() error {
	// scan the log directory and clean files last
	// accessed after an hour
	df, err := os.Open(l.playbackDir)
	if err != nil {
		return trace.ConvertSystemError(err)
	}
	defer df.Close()
	entries, err := df.Readdir(-1)
	if err != nil {
		return trace.ConvertSystemError(err)
	}
	for i := range entries {
		fi := entries[i]
		if fi.IsDir() {
			continue
		}
		fd := fi.ModTime().UTC()
		diff := l.Clock.Now().UTC().Sub(fd)
		if diff <= l.PlaybackRecycleTTL {
			continue
		}
		fileToRemove := filepath.Join(l.playbackDir, fi.Name())
		err := os.Remove(fileToRemove)
		if err != nil {
			l.log.Warningf("Failed to remove file %v: %v.", fileToRemove, err)
		}
		l.log.Debugf("Removed unpacked session playback file %v after %v.", fileToRemove, diff)
	}
	return nil
}

type readSeekCloser interface {
	io.Reader
	io.Seeker
	io.Closer
}

func (l *AuditLog) unpackFile(fileName string) (readSeekCloser, error) {
	basename := filepath.Base(fileName)
	unpackedFile := filepath.Join(l.playbackDir, strings.TrimSuffix(basename, filepath.Ext(basename)))

	// If client has called GetSessionChunk before session is over
	// this could lead to cases when not all data will be returned,
	// because unpackFile will be called concurrently with the unfinished write
	unpackedInfo, err := os.Stat(unpackedFile)
	err = trace.ConvertSystemError(err)
	switch {
	case err != nil && !trace.IsNotFound(err):
		return nil, trace.Wrap(err)
	case err == nil:
		packedInfo, err := os.Stat(fileName)
		if err != nil {
			return nil, trace.ConvertSystemError(err)
		}
		// no new data has been added
		if unpackedInfo.ModTime().Unix() >= packedInfo.ModTime().Unix() {
			return os.OpenFile(unpackedFile, os.O_RDONLY, 0640)
		}
	}

	start := l.Clock.Now()
	dest, err := os.OpenFile(unpackedFile, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0640)
	if err != nil {
		return nil, trace.ConvertSystemError(err)
	}
	source, err := os.OpenFile(fileName, os.O_RDONLY, 0640)
	if err != nil {
		return nil, trace.ConvertSystemError(err)
	}
	defer source.Close()
	reader, err := gzip.NewReader(source)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer reader.Close()
	if _, err := io.Copy(dest, reader); err != nil {
		// Unexpected EOF is returned by gzip reader
		// when the file has not been closed yet,
		// ignore this error
		if err != io.ErrUnexpectedEOF {
			dest.Close()
			return nil, trace.Wrap(err)
		}
	}
	if _, err := dest.Seek(0, 0); err != nil {
		dest.Close()
		return nil, trace.Wrap(err)
	}
	l.log.Debugf("Uncompressed %v into %v in %v", fileName, unpackedFile, l.Clock.Now().Sub(start))
	return dest, nil
}

func (l *AuditLog) getSessionChunk(namespace string, sid session.ID, offsetBytes, maxBytes int) ([]byte, error) {
	if namespace == "" {
		return nil, trace.BadParameter("missing parameter namespace")
	}
	idx, err := l.readSessionIndex(namespace, sid)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	fileName, fileOffset, err := idx.chunksFile(int64(offsetBytes))
	if err != nil {
		return nil, trace.Wrap(err)
	}
	reader, err := l.unpackFile(fileName)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer reader.Close()

	// seek to 'offset' from the beginning
	if _, err := reader.Seek(int64(offsetBytes)-fileOffset, 0); err != nil {
		return nil, trace.Wrap(err)
	}

	// copy up to maxBytes from the offset position:
	var buff bytes.Buffer
	_, err = io.Copy(&buff, io.LimitReader(reader, int64(maxBytes)))
	return buff.Bytes(), err
}

// Returns all events that happen during a session sorted by time
// (oldest first).
//
// Can be filtered by 'after' (cursor value to return events newer than)

func (l *AuditLog) GetSessionEvents(namespace string, sid session.ID, afterN int, includePrintEvents bool) ([]EventFields, error) {
	l.log.WithFields(log.Fields{"sid": string(sid), "afterN": afterN, "printEvents": includePrintEvents}).Debugf("GetSessionEvents.")
	if namespace == "" {
		return nil, trace.BadParameter("missing parameter namespace")
	}
	// Print events are stored in the context of the downloaded session
	// so pull them
	if !includePrintEvents && l.ExternalLog != nil {
		events, err := l.ExternalLog.GetSessionEvents(namespace, sid, afterN, includePrintEvents)
		// some loggers (e.g. FileLog) do not support retrieving session only print events,
		// in this case rely on local fallback to download the session,
		// unpack it and use local search
		if !trace.IsNotImplemented(err) {
			return events, err
		}
	}
	// If code has to fetch print events (for playback) it has to download
	// the playback from external storage first
	if err := l.downloadSession(namespace, sid); err != nil {
		return nil, trace.Wrap(err)
	}
	idx, err := l.readSessionIndex(namespace, sid)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	fileIndex, err := idx.eventsFile(afterN)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	events := make([]EventFields, 0, 256)
	for i := fileIndex; i < len(idx.events); i++ {
		skip := 0
		if i == fileIndex {
			skip = afterN
		}
		out, err := l.fetchSessionEvents(idx.eventsFileName(i), skip)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		events = append(events, out...)
	}
	return events, nil
}

func (l *AuditLog) fetchSessionEvents(fileName string, afterN int) ([]EventFields, error) {
	logFile, err := os.OpenFile(fileName, os.O_RDONLY, 0640)
	if err != nil {
		// no file found? this means no events have been logged yet
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, trace.Wrap(err)
	}
	defer logFile.Close()
	reader, err := gzip.NewReader(logFile)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer reader.Close()

	retval := make([]EventFields, 0, 256)
	// read line by line:
	scanner := bufio.NewScanner(reader)
	for lineNo := 0; scanner.Scan(); lineNo++ {
		if lineNo < afterN {
			continue
		}
		var fields EventFields
		if err = json.Unmarshal(scanner.Bytes(), &fields); err != nil {
			log.Error(err)
			return nil, trace.Wrap(err)
		}
		fields[EventCursor] = lineNo
		retval = append(retval, fields)
	}
	return retval, nil
}

// EmitAuditEvent adds a new event to the local file log
func (l *AuditLog) EmitAuditEvent(ctx context.Context, event apievents.AuditEvent) error {
	// If an external logger has been set, use it as the emitter, otherwise
	// fallback to the local disk based emitter.
	var emitAuditEvent func(ctx context.Context, event apievents.AuditEvent) error

	if l.ExternalLog != nil {
		emitAuditEvent = l.ExternalLog.EmitAuditEvent
	} else {
		emitAuditEvent = l.getLocalLog().EmitAuditEvent
	}
	err := emitAuditEvent(ctx, event)
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// auditDirs returns directories used for audit log storage
func (l *AuditLog) auditDirs() ([]string, error) {
	authServers, err := getAuthServers(l.DataDir)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	var out []string
	for _, serverID := range authServers {
		out = append(out, filepath.Join(l.DataDir, serverID))
	}
	return out, nil
}

func (l *AuditLog) SearchEvents(fromUTC, toUTC time.Time, namespace string, eventType []string, limit int, order types.EventOrder, startKey string) ([]apievents.AuditEvent, string, error) {
	g := l.log.WithFields(log.Fields{"namespace": namespace, "eventType": eventType, "limit": limit})
	g.Debugf("SearchEvents(%v, %v)", fromUTC, toUTC)
	if limit <= 0 {
		limit = defaults.EventsIterationLimit
	}
	if limit > defaults.EventsMaxIterationLimit {
		return nil, "", trace.BadParameter("limit %v exceeds max iteration limit %v", limit, defaults.MaxIterationLimit)
	}
	if l.ExternalLog != nil {
		return l.ExternalLog.SearchEvents(fromUTC, toUTC, namespace, eventType, limit, order, startKey)
	}
	return l.localLog.SearchEvents(fromUTC, toUTC, namespace, eventType, limit, order, startKey)
}

func (l *AuditLog) SearchSessionEvents(fromUTC, toUTC time.Time, limit int, order types.EventOrder, startKey string, cond *types.WhereExpr, sessionID string) ([]apievents.AuditEvent, string, error) {
	l.log.Debugf("SearchSessionEvents(%v, %v, %v)", fromUTC, toUTC, limit)
	if l.ExternalLog != nil {
		return l.ExternalLog.SearchSessionEvents(fromUTC, toUTC, limit, order, startKey, cond, sessionID)
	}
	return l.localLog.SearchSessionEvents(fromUTC, toUTC, limit, order, startKey, cond, sessionID)
}

// StreamSessionEvents streams all events from a given session recording. An error is returned on the first
// channel if one is encountered. Otherwise the event channel is closed when the stream ends.
// The event channel is not closed on error to prevent race conditions in downstream select statements.
func (l *AuditLog) StreamSessionEvents(ctx context.Context, sessionID session.ID, startIndex int64) (chan apievents.AuditEvent, chan error) {
	l.log.Debugf("StreamSessionEvents(%v)", sessionID)
	e := make(chan error, 1)
	c := make(chan apievents.AuditEvent)

	tarballPath := filepath.Join(l.playbackDir, string(sessionID)+".stream.tar")
	downloadCtx, cancel := l.createOrGetDownload(tarballPath)

	// Wait until another in progress download finishes and use its tarball.
	if cancel == nil {
		l.log.Debugf("Another download is in progress for %v, waiting until it gets completed.", sessionID)
		select {
		case <-downloadCtx.Done():
		case <-l.ctx.Done():
			e <- trace.BadParameter("audit log is closing, aborting the download")
			return c, e
		}
	} else {
		defer cancel()
	}

	rawSession, err := os.OpenFile(tarballPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0640)
	if err != nil {
		e <- trace.Wrap(err)
		return c, e
	}

	start := time.Now()
	if err := l.UploadHandler.Download(l.ctx, sessionID, rawSession); err != nil {
		// remove partially downloaded tarball
		if rmErr := os.Remove(tarballPath); rmErr != nil {
			l.log.WithError(rmErr).Warningf("Failed to remove file %v.", tarballPath)
		}

		e <- trace.Wrap(err)
		return c, e
	}

	l.log.WithField("duration", time.Since(start)).Debugf("Downloaded %v to %v.", sessionID, tarballPath)
	_, err = rawSession.Seek(0, 0)
	if err != nil {
		e <- trace.Wrap(err)
		return c, e
	}

	if err != nil {
		e <- trace.Wrap(err)
		return c, e
	}

	protoReader := NewProtoReader(rawSession)

	go func() {
		for {
			if ctx.Err() != nil {
				e <- trace.Wrap(ctx.Err())
				break
			}

			event, err := protoReader.Read(ctx)
			if err != nil {
				if err != io.EOF {
					e <- trace.Wrap(err)
				} else {
					close(c)
				}

				break
			}

			if event.GetIndex() >= startIndex {
				c <- event
			}
		}
	}()

	return c, e
}

// getLocalLog returns the local (file based) audit log.
func (l *AuditLog) getLocalLog() IAuditLog {
	l.RLock()
	defer l.RUnlock()

	// If no local log exists, which can occur during shutdown when the local log
	// has been set to "nil" by Close, return a nop audit log.
	if l.localLog == nil {
		return NewDiscardAuditLog()
	}
	return l.localLog
}

// Closes the audit log, which includes closing all file handles and releasing
// all session loggers
func (l *AuditLog) Close() error {
	if l.ExternalLog != nil {
		if err := l.ExternalLog.Close(); err != nil {
			log.Warningf("Close failure: %v", err)
		}
	}
	l.cancel()
	l.Lock()
	defer l.Unlock()

	if l.localLog != nil {
		if err := l.localLog.Close(); err != nil {
			log.Warningf("Close failure: %v", err)
		}
		l.localLog = nil
	}
	return nil
}

func (l *AuditLog) periodicCleanupPlaybacks() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-l.ctx.Done():
			return
		case <-ticker.C:
			if err := l.cleanupOldPlaybacks(); err != nil {
				l.log.Warningf("Error while cleaning up playback files: %v.", err)
			}
		}
	}
}

// periodicSpaceMonitor run forever monitoring how much disk space has been
// used on disk. Values are emitted to a Prometheus gauge.
func (l *AuditLog) periodicSpaceMonitor() {
	ticker := time.NewTicker(defaults.DiskAlertInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Find out what percentage of disk space is used. If the syscall fails,
			// emit that to prometheus as well.
			usedPercent, err := utils.PercentUsed(l.DataDir)
			if err != nil {
				auditFailedDisk.Inc()
				log.Warnf("Disk space monitoring failed: %v.", err)
				continue
			}

			// Update prometheus gauge with the percentage disk space used.
			auditDiskUsed.Set(usedPercent)

			// If used percentage goes above the alerting level, write to logs as well.
			if usedPercent > float64(defaults.DiskAlertThreshold) {
				log.Warnf("Free disk space for audit log is running low, %v%% of disk used.", usedPercent)
			}
		case <-l.ctx.Done():
			return
		}
	}
}
