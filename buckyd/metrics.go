package main

import (
	"encoding/json"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
)

import "github.com/jjneely/buckytools/fill"

// MetricStatType A JSON marshalable FileInfo type
type MetricStatType struct {
	Name    string // Filename
	Size    int64  // file size
	Mode    uint32 // mode bits
	ModTime int64  // Unix time
}

// listMetrics retrieves a list of metrics on the localhost and sends
// it to the client.
func listMetrics(w http.ResponseWriter, r *http.Request) {
	logRequest(r)
	// Check our methods.  We handle GET/POST.
	if r.Method != "GET" && r.Method != "POST" {
		http.Error(w, "Bad request method.", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "GET/POST parameter parsing error.", http.StatusBadRequest)
		return
	}

	// Do we need to init the metricsCache?
	if metricsCache == nil {
		metricsCache = NewMetricsCache()
	}

	// Handle case when we are currently building the cache
	if r.Form.Get("force") != "" && metricsCache.IsAvailable() {
		metricsCache.RefreshCache()
	}
	metrics, ok := metricsCache.GetMetrics()
	if !ok {
		http.Error(w, "Cache update in progress.", http.StatusAccepted)
		return
	}

	// Options
	if r.Form.Get("regex") != "" {
		m, err := FilterRegex(r.Form.Get("regex"), metrics)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		metrics = m
	}
	if r.Form.Get("list") != "" {
		filter, err := unmarshalList(r.Form.Get("list"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		metrics = FilterList(filter, metrics)
	}

	// Marshal the data back as a JSON list
	blob, err := json.Marshal(metrics)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		log.Printf("Error marshaling data: %s", err)
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.Write(blob)
	}
}

func serveMetrics(w http.ResponseWriter, r *http.Request) {
	logRequest(r)

	metric := r.URL.Path[len("/metrics/"):]
	path := MetricToPath(metric)
	if len(metric) == 0 {
		http.Error(w, "Metric name missing.", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case "HEAD":
		err := statMetric(w, metric, path)
		w.Header().Set("Content-Length", "0")
		status := http.StatusOK
		if err != nil {
			// XXX: Type switch and better error reporting
			status = http.StatusNotFound
		}
		w.WriteHeader(status)
		// HEAD seems to behave a bit differently, forcing the headers
		// seems to get the connection closed after the request.
	case "GET":
		// statMetric may return an error, which will be handled by
		// ServeFile() below.  Likely file not found.
		statMetric(w, metric, path)
		// XXX: This is a little HTML-specific but I think it
		// will work
		http.ServeFile(w, r, path)
	case "DELETE":
		// XXX: Auth?  Holodeck safeties are off!
		deleteMetric(w, path, true)
	case "PUT":
		// Replace metric data on disk
		// XXX: Metric will still be deleted if an error in heal occurrs
		err := deleteMetric(w, path, false)
		if err == nil {
			healMetric(w, r, path)
		}
	case "POST":
		// Backfill
		healMetric(w, r, path)
	default:
		http.Error(w, "Bad method request.", http.StatusBadRequest)
	}
}

// statMetric stat()s the given metric file system path and add the
// X-Metric-Stat header to the response as JSON encoded data
func statMetric(w http.ResponseWriter, metric, path string) error {
	s, err := os.Stat(path)
	if err != nil {
		return err
	}

	stat := new(MetricStatType)
	stat.Name = metric
	stat.Size = s.Size()
	stat.Mode = uint32(s.Mode())
	stat.ModTime = s.ModTime().Unix()

	// We should be able to marshal this struct without the funcs
	blob, err := json.Marshal(stat)
	if err != nil {
		return err
	}

	w.Header().Set("X-Metric-Stat", string(blob))
	return nil
}

// deleteMetric removes a metric DB from the file system and handles
// reporting any associated errors back to the client.  Set fatal to true
// to treat file not found as an error rather than success.
func deleteMetric(w http.ResponseWriter, path string, fatal bool) error {
	err := os.Remove(path)
	if err != nil {
		if os.IsNotExist(err) && fatal {
			http.Error(w, "Metric not found.", http.StatusNotFound)
			return err
		} else if !os.IsNotExist(err) {
			log.Printf("Error deleting metric %s: %s", path, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return err
		}
	}
	return nil
}

// healMetric will use the Whisper DB in the body of the request to
// backfill the metric found at the given filesystem path.  If the metric
// doesn't exist it will be created as an identical copy of the DB found
// in the request.
func healMetric(w http.ResponseWriter, r *http.Request, path string) {
	// Does this request look sane?
	if r.Header.Get("Content-Type") != "application/octet-stream" {
		http.Error(w, "Content-Type must be application/octet-stream.",
			http.StatusBadRequest)
		log.Printf("Got send a content-type of %s, abort!", r.Header.Get("Content-Type"))
		return
	}
	i, err := strconv.Atoi(r.Header.Get("Content-Length"))
	if err != nil || i <= 28 {
		// Whisper file headers are 28 bytes and we need data too.
		// Something is wrong here
		log.Printf("Whisper data in request too small: %d bytes", i)
		http.Error(w, "Whisper data in request too small.", http.StatusBadRequest)
	}

	// Does the destination path on dist exist?
	dstExists := true
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Error stat'ing file %s: %s", path, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		err := os.MkdirAll(filepath.Dir(path), 0755)
		if err != nil {
			log.Printf("Error creating %s: %s", filepath.Dir(path), err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		dstExists = false
	}

	// Write request body to a tmpfile
	fd, err := ioutil.TempFile(tmpDir, "buckyd")
	if err != nil {
		log.Printf("Error creating temp file: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, err = io.Copy(fd, r.Body)
	if err != nil {
		log.Printf("Error writing to temp file: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		fd.Close()
		os.Remove(fd.Name())
		return
	}
	srcName := fd.Name()
	fd.Sync()
	fd.Close()
	defer os.Remove(srcName) // not concerned with errors here

	// XXX: How can we check the tmpfile for sanity?
	if dstExists {
		err := fill.All(srcName, path)
		if err != nil {
			log.Printf("Error backfilling %s => %s: %s", srcName, path, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		src, err := os.Open(srcName)
		if err != nil {
			log.Printf("Error opening tmp file %s: %s", srcName, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer src.Close()
		dst, err := os.Create(path)
		if err != nil {
			log.Printf("Error opening metric file %s: %s", path, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer dst.Close()
		_, err = io.Copy(dst, src)
		if err != nil {
			log.Printf("Error copying %s => %s: %s", srcName, path, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

}