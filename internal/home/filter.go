package home

import (
	"bufio"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AdguardTeam/AdGuardHome/internal/filtering"
	"github.com/AdguardTeam/golibs/errors"
	"github.com/AdguardTeam/golibs/log"
	"github.com/AdguardTeam/golibs/stringutil"
)

var nextFilterID = time.Now().Unix() // semi-stable way to generate an unique ID

// Filtering - module object
type Filtering struct {
	// conf FilteringConf
	refreshStatus     uint32 // 0:none; 1:in progress
	refreshLock       sync.Mutex
	filterTitleRegexp *regexp.Regexp
}

// Init - initialize the module
func (f *Filtering) Init() {
	f.filterTitleRegexp = regexp.MustCompile(`^! Title: +(.*)$`)
	_ = os.MkdirAll(filepath.Join(Context.getDataDir(), filterDir), 0o755)
	f.loadFilters(config.Filters)
	f.loadFilters(config.WhitelistFilters)
	deduplicateFilters()
	updateUniqueFilterID(config.Filters)
	updateUniqueFilterID(config.WhitelistFilters)
}

// Start - start the module
func (f *Filtering) Start() {
	f.RegisterFilteringHandlers()

	// Here we should start updating filters,
	//  but currently we can't wake up the periodic task to do so.
	// So for now we just start this periodic task from here.
	go f.periodicallyRefreshFilters()
}

// Close - close the module
func (f *Filtering) Close() {
}

func defaultFilters() []filter {
	return []filter{
		{Filter: filtering.Filter{ID: 1}, Enabled: true, URL: "https://adguardteam.github.io/AdGuardSDNSFilter/Filters/filter.txt", Name: "AdGuard DNS filter"},
		{Filter: filtering.Filter{ID: 2}, Enabled: false, URL: "https://adaway.org/hosts.txt", Name: "AdAway Default Blocklist"},
	}
}

// field ordering is important -- yaml fields will mirror ordering from here
type filter struct {
	Enabled     bool
	URL         string    // URL or a file path
	Name        string    `yaml:"name"`
	RulesCount  int       `yaml:"-"`
	LastUpdated time.Time `yaml:"-"`
	checksum    uint32    // checksum of the file data
	white       bool

	filtering.Filter `yaml:",inline"`
}

const (
	statusFound          = 1
	statusEnabledChanged = 2
	statusURLChanged     = 4
	statusURLExists      = 8
	statusUpdateRequired = 0x10
)

// Update properties for a filter specified by its URL
// Return status* flags.
func (f *Filtering) filterSetProperties(url string, newf filter, whitelist bool) int {
	r := 0
	config.Lock()
	defer config.Unlock()

	filters := &config.Filters
	if whitelist {
		filters = &config.WhitelistFilters
	}

	for i := range *filters {
		filt := &(*filters)[i]
		if filt.URL != url {
			continue
		}

		log.Debug("filter: set properties: %s: {%s %s %v}",
			filt.URL, newf.Name, newf.URL, newf.Enabled)
		filt.Name = newf.Name

		if filt.URL != newf.URL {
			r |= statusURLChanged | statusUpdateRequired
			if filterExistsNoLock(newf.URL) {
				return statusURLExists
			}
			filt.URL = newf.URL
			filt.unload()
			filt.LastUpdated = time.Time{}
			filt.checksum = 0
			filt.RulesCount = 0
		}

		if filt.Enabled != newf.Enabled {
			r |= statusEnabledChanged
			filt.Enabled = newf.Enabled
			if filt.Enabled {
				if (r & statusURLChanged) == 0 {
					e := f.load(filt)
					if e != nil {
						// This isn't a fatal error,
						//  because it may occur when someone removes the file from disk.
						filt.LastUpdated = time.Time{}
						filt.checksum = 0
						filt.RulesCount = 0
						r |= statusUpdateRequired
					}
				}
			} else {
				filt.unload()
			}
		}

		return r | statusFound
	}
	return 0
}

// Return TRUE if a filter with this URL exists
func filterExists(url string) bool {
	config.RLock()
	r := filterExistsNoLock(url)
	config.RUnlock()
	return r
}

func filterExistsNoLock(url string) bool {
	for _, f := range config.Filters {
		if f.URL == url {
			return true
		}
	}
	for _, f := range config.WhitelistFilters {
		if f.URL == url {
			return true
		}
	}
	return false
}

// Add a filter
// Return FALSE if a filter with this URL exists
func filterAdd(f filter) bool {
	config.Lock()
	defer config.Unlock()

	// Check for duplicates
	if filterExistsNoLock(f.URL) {
		return false
	}

	if f.white {
		config.WhitelistFilters = append(config.WhitelistFilters, f)
	} else {
		config.Filters = append(config.Filters, f)
	}
	return true
}

// Load filters from the disk
// And if any filter has zero ID, assign a new one
func (f *Filtering) loadFilters(array []filter) {
	for i := range array {
		filter := &array[i] // otherwise we're operating on a copy
		if filter.ID == 0 {
			filter.ID = assignUniqueFilterID()
		}

		if !filter.Enabled {
			// No need to load a filter that is not enabled
			continue
		}

		err := f.load(filter)
		if err != nil {
			log.Error("Couldn't load filter %d contents due to %s", filter.ID, err)
		}
	}
}

func deduplicateFilters() {
	// Deduplicate filters
	i := 0 // output index, used for deletion later
	urls := map[string]bool{}
	for _, filter := range config.Filters {
		if _, ok := urls[filter.URL]; !ok {
			// we didn't see it before, keep it
			urls[filter.URL] = true // remember the URL
			config.Filters[i] = filter
			i++
		}
	}

	// all entries we want to keep are at front, delete the rest
	config.Filters = config.Filters[:i]
}

// Set the next filter ID to max(filter.ID) + 1
func updateUniqueFilterID(filters []filter) {
	for _, filter := range filters {
		if nextFilterID < filter.ID {
			nextFilterID = filter.ID + 1
		}
	}
}

func assignUniqueFilterID() int64 {
	value := nextFilterID
	nextFilterID++
	return value
}

// Sets up a timer that will be checking for filters updates periodically
func (f *Filtering) periodicallyRefreshFilters() {
	const maxInterval = 1 * 60 * 60
	intval := 5 // use a dynamically increasing time interval
	for {
		isNetworkErr := false
		if config.DNS.FiltersUpdateIntervalHours != 0 && atomic.CompareAndSwapUint32(&f.refreshStatus, 0, 1) {
			f.refreshLock.Lock()
			_, isNetworkErr = f.refreshFiltersIfNecessary(filterRefreshBlocklists | filterRefreshAllowlists)
			f.refreshLock.Unlock()
			f.refreshStatus = 0
			if !isNetworkErr {
				intval = maxInterval
			}
		}

		if isNetworkErr {
			intval *= 2
			if intval > maxInterval {
				intval = maxInterval
			}
		}

		time.Sleep(time.Duration(intval) * time.Second)
	}
}

// Refresh filters
// flags: filterRefresh*
// important:
//
// TRUE: ignore the fact that we're currently updating the filters
func (f *Filtering) refreshFilters(flags int, important bool) (int, error) {
	set := atomic.CompareAndSwapUint32(&f.refreshStatus, 0, 1)
	if !important && !set {
		return 0, fmt.Errorf("filters update procedure is already running")
	}

	f.refreshLock.Lock()
	nUpdated, _ := f.refreshFiltersIfNecessary(flags)
	f.refreshLock.Unlock()
	f.refreshStatus = 0
	return nUpdated, nil
}

func (f *Filtering) refreshFiltersArray(filters *[]filter, force bool) (int, []filter, []bool, bool) {
	var updateFilters []filter
	var updateFlags []bool // 'true' if filter data has changed

	now := time.Now()
	config.RLock()
	for i := range *filters {
		f := &(*filters)[i] // otherwise we will be operating on a copy

		if !f.Enabled {
			continue
		}

		expireTime := f.LastUpdated.Unix() + int64(config.DNS.FiltersUpdateIntervalHours)*60*60
		if !force && expireTime > now.Unix() {
			continue
		}

		var uf filter
		uf.ID = f.ID
		uf.URL = f.URL
		uf.Name = f.Name
		uf.checksum = f.checksum
		updateFilters = append(updateFilters, uf)
	}
	config.RUnlock()

	if len(updateFilters) == 0 {
		return 0, nil, nil, false
	}

	nfail := 0
	for i := range updateFilters {
		uf := &updateFilters[i]
		updated, err := f.update(uf)
		updateFlags = append(updateFlags, updated)
		if err != nil {
			nfail++
			log.Printf("Failed to update filter %s: %s\n", uf.URL, err)
			continue
		}
	}

	if nfail == len(updateFilters) {
		return 0, nil, nil, true
	}

	updateCount := 0
	for i := range updateFilters {
		uf := &updateFilters[i]
		updated := updateFlags[i]

		config.Lock()
		for k := range *filters {
			f := &(*filters)[k]
			if f.ID != uf.ID || f.URL != uf.URL {
				continue
			}
			f.LastUpdated = uf.LastUpdated
			if !updated {
				continue
			}

			log.Info("Updated filter #%d.  Rules: %d -> %d",
				f.ID, f.RulesCount, uf.RulesCount)
			f.Name = uf.Name
			f.RulesCount = uf.RulesCount
			f.checksum = uf.checksum
			updateCount++
		}
		config.Unlock()
	}

	return updateCount, updateFilters, updateFlags, false
}

const (
	filterRefreshForce      = 1 // ignore last file modification date
	filterRefreshAllowlists = 2 // update allow-lists
	filterRefreshBlocklists = 4 // update block-lists
)

// refreshFiltersIfNecessary checks filters and updates them if necessary.  If
// force is true, it ignores the filter.LastUpdated field value.
//
// Algorithm:
//
//  1. Get the list of filters to be updated.  For each filter, run the download
//     and checksum check operation.  Store downloaded data in a temporary file
//     inside data/filters directory
//
//  2. For each filter, if filter data hasn't changed, just set new update time
//     on file.  Otherwise, rename the temporary file (<temp> -> 1.txt).  Note
//     that this method works only on Unix systems.  On Windows, don't pass
//     files to filtering, pass the whole data.
//
// refreshFiltersIfNecessary returns the number of updated filters.  It also
// returns true if there was a network error and nothing could be updated.
//
// TODO(a.garipov, e.burkov): What the hell?
func (f *Filtering) refreshFiltersIfNecessary(flags int) (int, bool) {
	log.Debug("Filters: updating...")

	updateCount := 0
	var updateFilters []filter
	var updateFlags []bool
	netError := false
	netErrorW := false
	force := false
	if (flags & filterRefreshForce) != 0 {
		force = true
	}
	if (flags & filterRefreshBlocklists) != 0 {
		updateCount, updateFilters, updateFlags, netError = f.refreshFiltersArray(&config.Filters, force)
	}
	if (flags & filterRefreshAllowlists) != 0 {
		updateCountW := 0
		var updateFiltersW []filter
		var updateFlagsW []bool
		updateCountW, updateFiltersW, updateFlagsW, netErrorW = f.refreshFiltersArray(&config.WhitelistFilters, force)
		updateCount += updateCountW
		updateFilters = append(updateFilters, updateFiltersW...)
		updateFlags = append(updateFlags, updateFlagsW...)
	}
	if netError && netErrorW {
		return 0, true
	}

	if updateCount != 0 {
		enableFilters(false)

		for i := range updateFilters {
			uf := &updateFilters[i]
			updated := updateFlags[i]
			if !updated {
				continue
			}
			_ = os.Remove(uf.Path() + ".old")
		}
	}

	log.Debug("Filters: update finished")
	return updateCount, false
}

// Allows printable UTF-8 text with CR, LF, TAB characters
func isPrintableText(data []byte, len int) bool {
	for i := 0; i < len; i++ {
		c := data[i]
		if (c >= ' ' && c != 0x7f) || c == '\n' || c == '\r' || c == '\t' {
			continue
		}
		return false
	}
	return true
}

// A helper function that parses filter contents and returns a number of rules and a filter name (if there's any)
func (f *Filtering) parseFilterContents(file io.Reader) (int, uint32, string) {
	rulesCount := 0
	name := ""
	seenTitle := false
	r := bufio.NewReader(file)
	checksum := uint32(0)

	for {
		line, err := r.ReadString('\n')
		checksum = crc32.Update(checksum, crc32.IEEETable, []byte(line))

		line = strings.TrimSpace(line)
		if len(line) == 0 {
			//
		} else if line[0] == '!' {
			m := f.filterTitleRegexp.FindAllStringSubmatch(line, -1)
			if len(m) > 0 && len(m[0]) >= 2 && !seenTitle {
				name = m[0][1]
				seenTitle = true
			}

		} else if line[0] == '#' {
			//
		} else {
			rulesCount++
		}

		if err != nil {
			break
		}
	}

	return rulesCount, checksum, name
}

// Perform upgrade on a filter and update LastUpdated value
func (f *Filtering) update(filter *filter) (bool, error) {
	b, err := f.updateIntl(filter)
	filter.LastUpdated = time.Now()
	if !b {
		e := os.Chtimes(filter.Path(), filter.LastUpdated, filter.LastUpdated)
		if e != nil {
			log.Error("os.Chtimes(): %v", e)
		}
	}
	return b, err
}

func (f *Filtering) read(reader io.Reader, tmpFile *os.File, filter *filter) (int, error) {
	htmlTest := true
	firstChunk := make([]byte, 4*1024)
	firstChunkLen := 0
	buf := make([]byte, 64*1024)
	total := 0
	for {
		n, err := reader.Read(buf)
		total += n

		if htmlTest {
			num := len(firstChunk) - firstChunkLen
			if n < num {
				num = n
			}
			copied := copy(firstChunk[firstChunkLen:], buf[:num])
			firstChunkLen += copied

			if firstChunkLen == len(firstChunk) || err == io.EOF {
				if !isPrintableText(firstChunk, firstChunkLen) {
					return total, fmt.Errorf("data contains non-printable characters")
				}

				s := strings.ToLower(string(firstChunk))
				if strings.Contains(s, "<html") || strings.Contains(s, "<!doctype") {
					return total, fmt.Errorf("data is HTML, not plain text")
				}

				htmlTest = false
				firstChunk = nil
			}
		}

		_, err2 := tmpFile.Write(buf[:n])
		if err2 != nil {
			return total, err2
		}

		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			log.Printf("Couldn't fetch filter contents from URL %s, skipping: %s", filter.URL, err)
			return total, err
		}
	}
}

// finalizeUpdate closes and gets rid of temporary file f with filter's content
// according to updated.  It also saves new values of flt's name, rules number
// and checksum if sucсeeded.
func finalizeUpdate(
	f *os.File,
	flt *filter,
	updated bool,
	name string,
	rnum int,
	cs uint32,
) (err error) {
	tmpFileName := f.Name()

	// Close the file before renaming it because it's required on Windows.
	//
	// See https://github.com/adguardTeam/adGuardHome/issues/1553.
	if err = f.Close(); err != nil {
		return fmt.Errorf("closing temporary file: %w", err)
	}

	if !updated {
		log.Tracef("filter #%d from %s has no changes, skip", flt.ID, flt.URL)

		return os.Remove(tmpFileName)
	}

	log.Printf("saving filter %d contents to: %s", flt.ID, flt.Path())

	if err = os.Rename(tmpFileName, flt.Path()); err != nil {
		return errors.WithDeferred(err, os.Remove(tmpFileName))
	}

	flt.Name = stringutil.Coalesce(flt.Name, name)
	flt.checksum = cs
	flt.RulesCount = rnum

	return nil
}

// processUpdate copies filter's content from src to dst and returns the name,
// rules number, and checksum for it.  It also returns the number of bytes read
// from src.
func (f *Filtering) processUpdate(
	src io.Reader,
	dst *os.File,
	flt *filter,
) (name string, rnum int, cs uint32, n int, err error) {
	if n, err = f.read(src, dst, flt); err != nil {
		return "", 0, 0, 0, err
	}

	if _, err = dst.Seek(0, io.SeekStart); err != nil {
		return "", 0, 0, 0, err
	}

	rnum, cs, name = f.parseFilterContents(dst)

	return name, rnum, cs, n, nil
}

// updateIntl updates the flt rewriting it's actual file.  It returns true if
// the actual update has been performed.
func (f *Filtering) updateIntl(flt *filter) (ok bool, err error) {
	log.Tracef("downloading update for filter %d from %s", flt.ID, flt.URL)

	var name string
	var rnum, n int
	var cs uint32

	var tmpFile *os.File
	tmpFile, err = os.CreateTemp(filepath.Join(Context.getDataDir(), filterDir), "")
	if err != nil {
		return false, err
	}
	defer func() {
		err = errors.WithDeferred(err, finalizeUpdate(tmpFile, flt, ok, name, rnum, cs))
		ok = ok && err == nil
		if ok {
			log.Printf("updated filter %d: %d bytes, %d rules", flt.ID, n, rnum)
		}
	}()

	// Change the default 0o600 permission to something more acceptable by
	// end users.
	//
	// See https://github.com/AdguardTeam/AdGuardHome/issues/3198.
	if err = tmpFile.Chmod(0o644); err != nil {
		return false, fmt.Errorf("changing file mode: %w", err)
	}

	var r io.Reader
	if filepath.IsAbs(flt.URL) {
		var file io.ReadCloser
		file, err = os.Open(flt.URL)
		if err != nil {
			return false, fmt.Errorf("open file: %w", err)
		}
		defer func() { err = errors.WithDeferred(err, file.Close()) }()

		r = file
	} else {
		var resp *http.Response
		resp, err = Context.client.Get(flt.URL)
		if err != nil {
			log.Printf("requesting filter from %s, skip: %s", flt.URL, err)

			return false, err
		}
		defer func() { err = errors.WithDeferred(err, resp.Body.Close()) }()

		if resp.StatusCode != http.StatusOK {
			log.Printf("got status code %d from %s, skip", resp.StatusCode, flt.URL)

			return false, fmt.Errorf("got status code != 200: %d", resp.StatusCode)
		}

		r = resp.Body
	}

	name, rnum, cs, n, err = f.processUpdate(r, tmpFile, flt)

	return cs != flt.checksum, err
}

// loads filter contents from the file in dataDir
func (f *Filtering) load(filter *filter) (err error) {
	filterFilePath := filter.Path()

	log.Tracef("filtering: loading filter %d contents to: %s", filter.ID, filterFilePath)

	file, err := os.Open(filterFilePath)
	if errors.Is(err, os.ErrNotExist) {
		// Do nothing, file doesn't exist.
		return nil
	} else if err != nil {
		return fmt.Errorf("opening filter file: %w", err)
	}
	defer func() { err = errors.WithDeferred(err, file.Close()) }()

	st, err := file.Stat()
	if err != nil {
		return fmt.Errorf("getting filter file stat: %w", err)
	}

	log.Tracef("filtering: File %s, id %d, length %d", filterFilePath, filter.ID, st.Size())

	rulesCount, checksum, _ := f.parseFilterContents(file)

	filter.RulesCount = rulesCount
	filter.checksum = checksum
	filter.LastUpdated = st.ModTime()

	return nil
}

// Clear filter rules
func (filter *filter) unload() {
	filter.RulesCount = 0
	filter.checksum = 0
}

// Path to the filter contents
func (filter *filter) Path() string {
	return filepath.Join(Context.getDataDir(), filterDir, strconv.FormatInt(filter.ID, 10)+".txt")
}

func enableFilters(async bool) {
	config.RLock()
	defer config.RUnlock()

	enableFiltersLocked(async)
}

func enableFiltersLocked(async bool) {
	filters := []filtering.Filter{{
		ID:   filtering.CustomListID,
		Data: []byte(strings.Join(config.UserRules, "\n")),
	}}

	for _, filter := range config.Filters {
		if !filter.Enabled {
			continue
		}

		filters = append(filters, filtering.Filter{
			ID:       filter.ID,
			FilePath: filter.Path(),
		})
	}

	var allowFilters []filtering.Filter
	for _, filter := range config.WhitelistFilters {
		if !filter.Enabled {
			continue
		}

		allowFilters = append(allowFilters, filtering.Filter{
			ID:       filter.ID,
			FilePath: filter.Path(),
		})
	}

	if err := Context.dnsFilter.SetFilters(filters, allowFilters, async); err != nil {
		log.Debug("enabling filters: %s", err)
	}

	Context.dnsFilter.SetEnabled(config.DNS.FilteringEnabled)
}
