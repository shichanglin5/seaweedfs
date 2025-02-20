package weed_server

import (
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/security"
	"github.com/seaweedfs/seaweedfs/weed/util"

	"github.com/seaweedfs/seaweedfs/weed/stats"
)

func (fs *FilerServer) filerHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	statusRecorder := stats.NewStatusResponseWriter(w)
	w = statusRecorder
	origin := r.Header.Get("Origin")
	if origin != "" {
		if fs.option.AllowedOrigins == nil || len(fs.option.AllowedOrigins) == 0 || fs.option.AllowedOrigins[0] == "*" {
			origin = "*"
		} else {
			originFound := false
			for _, allowedOrigin := range fs.option.AllowedOrigins {
				if origin == allowedOrigin {
					originFound = true
				}
			}
			if !originFound {
				writeJsonError(w, r, http.StatusForbidden, errors.New("origin not allowed"))
				return
			}
		}

		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Expose-Headers", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "PUT, POST, GET, DELETE, OPTIONS")
	}

	if r.Method == "OPTIONS" {
		OptionsHandler(w, r, false)
		return
	}

	// proxy to volume servers
	var fileId string
	if strings.HasPrefix(r.RequestURI, "/?proxyChunkId=") {
		fileId = r.RequestURI[len("/?proxyChunkId="):]
	}
	if fileId != "" {
		fs.proxyToVolumeServer(w, r, fileId)
		stats.FilerHandlerCounter.WithLabelValues(stats.ChunkProxy).Inc()
		stats.FilerRequestHistogram.WithLabelValues(stats.ChunkProxy).Observe(time.Since(start).Seconds())
		return
	}

	defer func() {
		stats.FilerRequestCounter.WithLabelValues(r.Method, strconv.Itoa(statusRecorder.Status)).Inc()
		stats.FilerRequestHistogram.WithLabelValues(r.Method).Observe(time.Since(start).Seconds())
	}()

	isReadHttpCall := r.Method == "GET" || r.Method == "HEAD"
	if !fs.maybeCheckJwtAuthorization(r, !isReadHttpCall) {
		writeJsonError(w, r, http.StatusUnauthorized, errors.New("wrong jwt"))
		return
	}

	w.Header().Set("Server", "SeaweedFS Filer "+util.VERSION)

	switch r.Method {
	case "GET":
		fs.GetOrHeadHandler(w, r)
	case "HEAD":
		fs.GetOrHeadHandler(w, r)
	case "DELETE":
		if _, ok := r.URL.Query()["tagging"]; ok {
			fs.DeleteTaggingHandler(w, r)
		} else {
			fs.DeleteHandler(w, r)
		}
	case "POST", "PUT":
		// wait until in flight data is less than the limit
		contentLength := getContentLength(r)
		fs.inFlightDataLimitCond.L.Lock()
		inFlightDataSize := atomic.LoadInt64(&fs.inFlightDataSize)
		for fs.option.ConcurrentUploadLimit != 0 && inFlightDataSize > fs.option.ConcurrentUploadLimit {
			glog.V(4).Infof("wait because inflight data %d > %d", inFlightDataSize, fs.option.ConcurrentUploadLimit)
			fs.inFlightDataLimitCond.Wait()
			inFlightDataSize = atomic.LoadInt64(&fs.inFlightDataSize)
		}
		fs.inFlightDataLimitCond.L.Unlock()
		atomic.AddInt64(&fs.inFlightDataSize, contentLength)
		defer func() {
			atomic.AddInt64(&fs.inFlightDataSize, -contentLength)
			fs.inFlightDataLimitCond.Signal()
		}()

		if r.Method == "PUT" {
			if _, ok := r.URL.Query()["tagging"]; ok {
				fs.PutTaggingHandler(w, r)
			} else {
				fs.PostHandler(w, r, contentLength)
			}
		} else { // method == "POST"
			fs.PostHandler(w, r, contentLength)
		}
	}
}

func (fs *FilerServer) readonlyFilerHandler(w http.ResponseWriter, r *http.Request) {

	start := time.Now()
	statusRecorder := stats.NewStatusResponseWriter(w)
	w = statusRecorder

	os.Stdout.WriteString("Request: " + r.Method + " " + r.URL.String() + "\n")

	origin := r.Header.Get("Origin")
	if origin != "" {
		if fs.option.AllowedOrigins == nil || len(fs.option.AllowedOrigins) == 0 || fs.option.AllowedOrigins[0] == "*" {
			origin = "*"
		} else {
			originFound := false
			for _, allowedOrigin := range fs.option.AllowedOrigins {
				if origin == allowedOrigin {
					originFound = true
				}
			}
			if !originFound {
				writeJsonError(w, r, http.StatusForbidden, errors.New("origin not allowed"))
				return
			}
		}

		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Headers", "OPTIONS, GET, HEAD")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
	}

	defer func() {
		stats.FilerRequestCounter.WithLabelValues(r.Method, strconv.Itoa(statusRecorder.Status)).Inc()
		stats.FilerRequestHistogram.WithLabelValues(r.Method).Observe(time.Since(start).Seconds())
	}()
	// We handle OPTIONS first because it never should be authenticated
	if r.Method == "OPTIONS" {
		OptionsHandler(w, r, true)
		return
	}

	if !fs.maybeCheckJwtAuthorization(r, false) {
		writeJsonError(w, r, http.StatusUnauthorized, errors.New("wrong jwt"))
		return
	}

	w.Header().Set("Server", "SeaweedFS Filer "+util.VERSION)

	switch r.Method {
	case "GET":
		fs.GetOrHeadHandler(w, r)
	case "HEAD":
		fs.GetOrHeadHandler(w, r)
	}
}

func OptionsHandler(w http.ResponseWriter, r *http.Request, isReadOnly bool) {
	if isReadOnly {
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	} else {
		w.Header().Set("Access-Control-Allow-Methods", "PUT, POST, GET, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Expose-Headers", "*")
	}
	w.Header().Set("Access-Control-Allow-Headers", "*")
	w.Header().Set("Access-Control-Allow-Credentials", "true")
}

// maybeCheckJwtAuthorization returns true if access should be granted, false if it should be denied
func (fs *FilerServer) maybeCheckJwtAuthorization(r *http.Request, isWrite bool) bool {

	var signingKey security.SigningKey

	if isWrite {
		if len(fs.filerGuard.SigningKey) == 0 {
			return true
		} else {
			signingKey = fs.filerGuard.SigningKey
		}
	} else {
		if len(fs.filerGuard.ReadSigningKey) == 0 {
			return true
		} else {
			signingKey = fs.filerGuard.ReadSigningKey
		}
	}

	tokenStr := security.GetJwt(r)
	if tokenStr == "" {
		glog.V(1).Infof("missing jwt from %s", r.RemoteAddr)
		return false
	}

	token, err := security.DecodeJwt(signingKey, tokenStr, &security.SeaweedFilerClaims{})
	if err != nil {
		glog.V(1).Infof("jwt verification error from %s: %v", r.RemoteAddr, err)
		return false
	}
	if !token.Valid {
		glog.V(1).Infof("jwt invalid from %s: %v", r.RemoteAddr, tokenStr)
		return false
	} else {
		return true
	}
}

func (fs *FilerServer) filerHealthzHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Server", "SeaweedFS Filer "+util.VERSION)
	w.WriteHeader(http.StatusOK)
}
