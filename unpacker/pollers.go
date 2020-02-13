package unpacker

import (
	"log"
	"path/filepath"
	"time"
)

// torrent is what we care about. no usenet..
const torrent = "torrent"

// PollSonarr saves the Sonarr Queue to r.SonarrQ
func (u *Unpackerr) PollSonarr() error {
	var err error

	u.SonarrQ.Lock()
	defer u.SonarrQ.Unlock()

	if u.SonarrQ.List, err = u.Sonarr.SonarrQueue(); err != nil {
		return err
	}

	log.Println("Sonarr Updated:", len(u.SonarrQ.List), "Items Queued")

	return nil
}

// PollRadarr saves the Radarr Queue to r.RadarrQ
func (u *Unpackerr) PollRadarr() error {
	var err error

	u.RadarrQ.Lock()
	defer u.RadarrQ.Unlock()

	if u.RadarrQ.List, err = u.Radarr.RadarrQueue(); err != nil {
		return err
	}

	log.Println("Radarr Updated:", len(u.RadarrQ.List), "Items Queued")

	return nil
}

// PollChange runs other tasks.
// Those tasks: a) look for things to extract, b) look for things to delete.
// This runs more often because of the cleanup tasks.
// It doesn't poll external data, unless it finds something to extract.
func (u *Unpackerr) PollChange() {
	u.DeLogf("Starting Cleanup Routine (interval: 1 minute)")

	ticker := time.NewTicker(time.Minute)

	for {
		select {
		case <-ticker.C:
			u.CheckExtractDone()
			u.CheckSonarrQueue()
			u.CheckRadarrQueue()
		case <-u.StopChan:
			ticker.Stop()
			return
		}
	}
}

// CheckExtractDone checks if an extracted item has been imported.
func (u *Unpackerr) CheckExtractDone() {
	u.History.RLock()

	defer func() {
		u.History.RUnlock()
		ec := u.eCount()
		log.Printf("Extract Statuses: %d extracting, %d queued, "+
			"%d extracted, %d imported, %d failed, %d deleted. Finished: %d",
			ec.extracting, ec.queued, ec.extracted,
			ec.imported, ec.failed, ec.deleted, ec.finished)
	}()

	for name, data := range u.History.Map {
		u.DeLogf("Extract Status: %v (status: %v, elapsed: %v)", name, data.Status.String(),
			time.Since(data.Updated).Round(time.Second))

		switch {
		case data.Status >= DELETED && time.Since(data.Updated) >= u.DeleteDelay.Duration*2:
			// Remove the item from history some time after it's deleted.
			go u.finishFinished(data.App, name)
		case data.Status < EXTRACTED || data.Status > IMPORTED:
			continue // Only process items that have finished extraction and are not deleted.
		case data.App == "Sonarr":
			go u.handleSonarr(data, name)
		case data.App == "Radarr":
			go u.handleRadarr(data, name)
		}
	}
}

func (u *Unpackerr) finishFinished(app, name string) {
	u.History.Lock()
	defer u.History.Unlock()
	u.History.Finished++

	log.Printf("%v: Finished, Removing History: %v", app, name)
	delete(u.History.Map, name)
}

func (u *Unpackerr) handleRadarr(data Extracts, name string) {
	u.History.Lock()
	defer u.History.Unlock()

	if item := u.getRadarQitem(name); item.Status != "" {
		u.DeLogf("%s Item Waiting For Import (%s): %v -> %v", data.App, item.Protocol, name, item.Status)
		return // We only want finished items.
	} else if item.Protocol != torrent && item.Protocol != "" {
		return // We only want torrents.
	}

	if s := u.HandleExtractDone(data, name); s != data.Status {
		// Status changed.
		data.Status, data.Updated = s, time.Now()
		u.History.Map[name] = data
	}
}

func (u *Unpackerr) handleSonarr(data Extracts, name string) {
	u.History.Lock()
	defer u.History.Unlock()

	if item := u.getSonarQitem(name); item.Status != "" {
		u.DeLogf("%s Item Waiting For Import (%s): %v -> %v", data.App, item.Protocol, name, item.Status)
		return // We only want finished items.
	} else if item.Protocol != torrent && item.Protocol != "" {
		return // We only want torrents.
	}

	if s := u.HandleExtractDone(data, name); s != data.Status {
		data.Status, data.Updated = s, time.Now()
		u.History.Map[name] = data
	}
}

// HandleExtractDone checks if files should be deleted.
func (u *Unpackerr) HandleExtractDone(data Extracts, name string) ExtractStatus {
	switch elapsed := time.Since(data.Updated); {
	case data.Status != IMPORTED:
		log.Printf("%v Imported: %v (delete in %v)", data.App, name, u.DeleteDelay)
		return IMPORTED
	case elapsed >= u.DeleteDelay.Duration:
		deleteFiles(data.Files)
		return DELETED
	default:
		u.DeLogf("%v: Awaiting Delete Delay (%v remains): %v",
			data.App, u.DeleteDelay.Duration-elapsed.Round(time.Second), name)
		return data.Status
	}
}

// CheckSonarrQueue passes completed Sonarr-queued downloads to the HandleCompleted method.
func (u *Unpackerr) CheckSonarrQueue() {
	u.SonarrQ.RLock()
	defer u.SonarrQ.RUnlock()

	for _, q := range u.SonarrQ.List {
		if q.Status == "Completed" && q.Protocol == torrent {
			go u.HandleCompleted(q.Title, "Sonarr", u.Config.SonarrPath)
		} else {
			u.DeLogf("Sonarr: %s (%s:%d%%): %v (Ep: %v)",
				q.Status, q.Protocol, int(100-(q.Sizeleft/q.Size*100)), q.Title, q.Episode.Title)
		}
	}
}

// CheckRadarrQueue passes completed Radarr-queued downloads to the HandleCompleted method.
func (u *Unpackerr) CheckRadarrQueue() {
	u.RadarrQ.RLock()
	defer u.RadarrQ.RUnlock()

	for _, q := range u.RadarrQ.List {
		if q.Status == "Completed" && q.Protocol == torrent {
			go u.HandleCompleted(q.Title, "Radarr", u.Config.RadarrPath)
		} else {
			u.DeLogf("Radarr: %s (%s:%d%%): %v",
				q.Status, q.Protocol, int(100-(q.Sizeleft/q.Size*100)), q.Title)
		}
	}
}

func (u *Unpackerr) historyExists(name string) (ok bool) {
	u.History.RLock()
	defer u.History.RUnlock()
	_, ok = u.History.Map[name]

	return
}

// HandleCompleted checks if a completed sonarr or radarr item needs to be extracted.
func (u *Unpackerr) HandleCompleted(name, app, path string) {
	path = filepath.Join(path, name)
	files := FindRarFiles(path)

	if !u.historyExists(name) {
		if len(files) > 0 {
			log.Printf("%s: Found %d extractable item(s): %s (%s)", app, len(files), name, path)
			u.CreateStatus(name, path, app, files)
			u.extractFiles(name, path, files)
		} else {
			u.DeLogf("%s: Completed item still in queue: %s, no extractable files found at: %s", app, name, path)
		}
	}
}