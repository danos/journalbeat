package prospector

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/elastic/beats/filebeat/channel"
	"github.com/elastic/beats/filebeat/harvester"
	"github.com/elastic/beats/filebeat/input/file"
	"github.com/elastic/beats/filebeat/util"
	"github.com/elastic/beats/libbeat/common"
	"github.com/elastic/beats/libbeat/logp"
	"github.com/elastic/beats/libbeat/monitoring"
)

const (
	recursiveGlobDepth = 8
)

var (
	filesRenamed   = monitoring.NewInt(nil, "filebeat.prospector.log.files.renamed")
	filesTruncated = monitoring.NewInt(nil, "filebeat.prospector.log.files.truncated")
)

// Log contains the prospector and its config
type Log struct {
	cfg      *common.Config
	config   prospectorConfig
	states   *file.States
	registry *harvesterRegistry
	outlet   channel.Outleter
	done     chan struct{}
}

// NewLog instantiates a new Log
func NewLog(cfg *common.Config, states []file.State, registry *harvesterRegistry, outlet channel.Outleter, done chan struct{}) (*Log, error) {

	prospectorer := &Log{
		cfg:      cfg,
		registry: registry,
		outlet:   outlet,
		states:   &file.States{},
		done:     done,
	}

	if err := cfg.Unpack(&prospectorer.config); err != nil {
		return nil, err
	}

	// Create empty harvester to check if configs are fine
	// TODO: Do config validation instead
	_, err := prospectorer.createHarvester(file.State{})
	if err != nil {
		return nil, err
	}

	if len(prospectorer.config.Paths) == 0 {
		return nil, fmt.Errorf("each prospector must have at least one path defined")
	}

	err = prospectorer.loadStates(states)
	if err != nil {
		return nil, err
	}

	logp.Debug("prospector", "File Configs: %v", prospectorer.config.Paths)

	return prospectorer, nil
}

// LoadStates loads states into prospector
// It goes through all states coming from the registry. Only the states which match the glob patterns of
// the prospector will be loaded and updated. All other states will not be touched.
func (l *Log) loadStates(states []file.State) error {
	logp.Debug("prospector", "exclude_files: %s", l.config.ExcludeFiles)

	for _, state := range states {
		// Check if state source belongs to this prospector. If yes, update the state.
		if l.matchesFile(state.Source) {
			state.TTL = -1

			// In case a prospector is tried to be started with an unfinished state matching the glob pattern
			if !state.Finished {
				return fmt.Errorf("Can only start a prospector when all related states are finished: %+v", state)
			}

			// Update prospector states and send new states to registry
			err := l.updateState(state)
			if err != nil {
				logp.Err("Problem putting initial state: %+v", err)
				return err
			}
		}
	}

	logp.Info("Prospector with previous states loaded: %v", l.states.Count())
	return nil
}

// Run runs the prospector
func (l *Log) Run() {
	logp.Debug("prospector", "Start next scan")

	// TailFiles is like ignore_older = 1ns and only on startup
	if l.config.TailFiles {
		ignoreOlder := l.config.IgnoreOlder

		// Overwrite ignore_older for the first scan
		l.config.IgnoreOlder = 1
		defer func() {
			// Reset ignore_older after first run
			l.config.IgnoreOlder = ignoreOlder
			// Disable tail_files after the first run
			l.config.TailFiles = false
		}()
	}
	l.scan()

	// It is important that a first scan is run before cleanup to make sure all new states are read first
	if l.config.CleanInactive > 0 || l.config.CleanRemoved {
		beforeCount := l.states.Count()
		cleanedStates := l.states.Cleanup()
		logp.Debug("prospector", "Prospector states cleaned up. Before: %d, After: %d", beforeCount, beforeCount-cleanedStates)
	}

	// Marking removed files to be cleaned up. Cleanup happens after next scan to make sure all states are updated first
	if l.config.CleanRemoved {
		for _, state := range l.states.GetStates() {
			// os.Stat will return an error in case the file does not exist
			stat, err := os.Stat(state.Source)
			if err != nil {
				if os.IsNotExist(err) {
					l.removeState(state)
					logp.Debug("prospector", "Remove state for file as file removed: %s", state.Source)
				} else {
					logp.Err("Prospector state for %s was not removed: %s", state.Source, err)
				}
			} else {
				// Check if existing source on disk and state are the same. Remove if not the case.
				newState := file.NewState(stat, state.Source, l.config.InputType)
				if !newState.FileStateOS.IsSame(state.FileStateOS) {
					l.removeState(state)
					logp.Debug("prospector", "Remove state for file as file removed or renamed: %s", state.Source)
				}
			}
		}
	}
}

func (l *Log) removeState(state file.State) {

	// Only clean up files where state is Finished
	if !state.Finished {
		logp.Debug("prospector", "State for file not removed because harvester not finished: %s", state.Source)
		return
	}

	state.TTL = 0
	err := l.updateState(state)
	if err != nil {
		logp.Err("File cleanup state update error: %s", err)
	}

}

// getFiles returns all files which have to be harvested
// All globs are expanded and then directory and excluded files are removed
func (l *Log) getFiles() map[string]os.FileInfo {

	paths := map[string]os.FileInfo{}

	for _, path := range l.config.Paths {
		depth := uint8(0)
		if l.config.recursiveGlob {
			depth = recursiveGlobDepth
		}
		matches, err := file.Glob(path, depth)
		if err != nil {
			logp.Err("glob(%s) failed: %v", path, err)
			continue
		}

	OUTER:
		// Check any matched files to see if we need to start a harvester
		for _, file := range matches {

			// check if the file is in the exclude_files list
			if l.isFileExcluded(file) {
				logp.Debug("prospector", "Exclude file: %s", file)
				continue
			}

			// Fetch Lstat File info to detected also symlinks
			fileInfo, err := os.Lstat(file)
			if err != nil {
				logp.Debug("prospector", "lstat(%s) failed: %s", file, err)
				continue
			}

			if fileInfo.IsDir() {
				logp.Debug("prospector", "Skipping directory: %s", file)
				continue
			}

			isSymlink := fileInfo.Mode()&os.ModeSymlink > 0
			if isSymlink && !l.config.Symlinks {
				logp.Debug("prospector", "File %s skipped as it is a symlink.", file)
				continue
			}

			// Fetch Stat file info which fetches the inode. In case of a symlink, the original inode is fetched
			fileInfo, err = os.Stat(file)
			if err != nil {
				logp.Debug("prospector", "stat(%s) failed: %s", file, err)
				continue
			}

			// If symlink is enabled, it is checked that original is not part of same prospector
			// It original is harvested by other prospector, states will potentially overwrite each other
			if l.config.Symlinks {
				for _, finfo := range paths {
					if os.SameFile(finfo, fileInfo) {
						logp.Info("Same file found as symlink and original. Skipping file: %s", file)
						continue OUTER
					}
				}
			}

			paths[file] = fileInfo
		}
	}

	return paths
}

// matchesFile returns true in case the given filePath is part of this prospector, means matches its glob patterns
func (l *Log) matchesFile(filePath string) bool {

	// Path is cleaned to ensure we always compare clean paths
	filePath = filepath.Clean(filePath)

	for _, glob := range l.config.Paths {

		// Glob is cleaned to ensure we always compare clean paths
		glob = filepath.Clean(glob)

		// Evaluate if glob matches filePath
		match, err := filepath.Match(glob, filePath)
		if err != nil {
			logp.Debug("prospector", "Error matching glob: %s", err)
			continue
		}

		// Check if file is not excluded
		if match && !l.isFileExcluded(filePath) {
			return true
		}
	}
	return false
}

// Scan starts a scanGlob for each provided path/glob
func (l *Log) scan() {

	for path, info := range l.getFiles() {

		select {
		case <-l.done:
			logp.Info("Scan aborted because prospector stopped.")
			return
		default:
		}

		var err error
		path, err = filepath.Abs(path)
		if err != nil {
			logp.Err("could not fetch abs path for file %s: %s", path, err)
		}
		logp.Debug("prospector", "Check file for harvesting: %s", path)

		// Create new state for comparison
		newState := file.NewState(info, path, l.config.InputType)

		// Load last state
		lastState := l.states.FindPrevious(newState)

		// Ignores all files which fall under ignore_older
		if l.isIgnoreOlder(newState) {
			err := l.handleIgnoreOlder(lastState, newState)
			if err != nil {
				logp.Err("Updating ignore_older state error: %s", err)
			}
			continue
		}

		// Decides if previous state exists
		if lastState.IsEmpty() {
			logp.Debug("prospector", "Start harvester for new file: %s", newState.Source)
			err := l.startHarvester(newState, 0)
			if err != nil {
				logp.Err("Harvester could not be started on new file: %s, Err: %s", newState.Source, err)
			}
		} else {
			l.harvestExistingFile(newState, lastState)
		}
	}
}

// harvestExistingFile continues harvesting a file with a known state if needed
func (l *Log) harvestExistingFile(newState file.State, oldState file.State) {

	logp.Debug("prospector", "Update existing file for harvesting: %s, offset: %v", newState.Source, oldState.Offset)

	// No harvester is running for the file, start a new harvester
	// It is important here that only the size is checked and not modification time, as modification time could be incorrect on windows
	// https://blogs.technet.microsoft.com/asiasupp/2010/12/14/file-date-modified-property-are-not-updating-while-modifying-a-file-without-closing-it/
	if oldState.Finished && newState.Fileinfo.Size() > oldState.Offset {
		// Resume harvesting of an old file we've stopped harvesting from
		// This could also be an issue with force_close_older that a new harvester is started after each scan but not needed?
		// One problem with comparing modTime is that it is in seconds, and scans can happen more then once a second
		logp.Debug("prospector", "Resuming harvesting of file: %s, offset: %v", newState.Source, oldState.Offset)
		err := l.startHarvester(newState, oldState.Offset)
		if err != nil {
			logp.Err("Harvester could not be started on existing file: %s, Err: %s", newState.Source, err)
		}
		return
	}

	// File size was reduced -> truncated file
	if oldState.Finished && newState.Fileinfo.Size() < oldState.Offset {
		logp.Debug("prospector", "Old file was truncated. Starting from the beginning: %s", newState.Source)
		err := l.startHarvester(newState, 0)
		if err != nil {
			logp.Err("Harvester could not be started on truncated file: %s, Err: %s", newState.Source, err)
		}

		filesTruncated.Add(1)
		return
	}

	// Check if file was renamed
	if oldState.Source != "" && oldState.Source != newState.Source {
		// This does not start a new harvester as it is assume that the older harvester is still running
		// or no new lines were detected. It sends only an event status update to make sure the new name is persisted.
		logp.Debug("prospector", "File rename was detected: %s -> %s, Current offset: %v", oldState.Source, newState.Source, oldState.Offset)

		if oldState.Finished {
			logp.Debug("prospector", "Updating state for renamed file: %s -> %s, Current offset: %v", oldState.Source, newState.Source, oldState.Offset)
			// Update state because of file rotation
			oldState.Source = newState.Source
			err := l.updateState(oldState)
			if err != nil {
				logp.Err("File rotation state update error: %s", err)
			}

			filesRenamed.Add(1)
		} else {
			logp.Debug("prospector", "File rename detected but harvester not finished yet.")
		}
	}

	if !oldState.Finished {
		// Nothing to do. Harvester is still running and file was not renamed
		logp.Debug("prospector", "Harvester for file is still running: %s", newState.Source)
	} else {
		logp.Debug("prospector", "File didn't change: %s", newState.Source)
	}
}

// handleIgnoreOlder handles states which fall under ignore older
// Based on the state information it is decided if the state information has to be updated or not
func (l *Log) handleIgnoreOlder(lastState, newState file.State) error {
	logp.Debug("prospector", "Ignore file because ignore_older reached: %s", newState.Source)

	if !lastState.IsEmpty() {
		if !lastState.Finished {
			logp.Info("File is falling under ignore_older before harvesting is finished. Adjust your close_* settings: %s", newState.Source)
		}
		// Old state exist, no need to update it
		return nil
	}

	// Make sure file is not falling under clean_inactive yet
	if l.isCleanInactive(newState) {
		logp.Debug("prospector", "Do not write state for ignore_older because clean_inactive reached")
		return nil
	}

	// Set offset to end of file to be consistent with files which were harvested before
	// See https://github.com/elastic/beats/pull/2907
	newState.Offset = newState.Fileinfo.Size()

	// Write state for ignore_older file as none exists yet
	newState.Finished = true
	err := l.updateState(newState)
	if err != nil {
		return err
	}

	return nil
}

// isFileExcluded checks if the given path should be excluded
func (l *Log) isFileExcluded(file string) bool {
	patterns := l.config.ExcludeFiles
	return len(patterns) > 0 && harvester.MatchAny(patterns, file)
}

// isIgnoreOlder checks if the given state reached ignore_older
func (l *Log) isIgnoreOlder(state file.State) bool {

	// ignore_older is disable
	if l.config.IgnoreOlder == 0 {
		return false
	}

	modTime := state.Fileinfo.ModTime()
	if time.Since(modTime) > l.config.IgnoreOlder {
		return true
	}

	return false
}

// isCleanInactive checks if the given state false under clean_inactive
func (l *Log) isCleanInactive(state file.State) bool {

	// clean_inactive is disable
	if l.config.CleanInactive <= 0 {
		return false
	}

	modTime := state.Fileinfo.ModTime()
	if time.Since(modTime) > l.config.CleanInactive {
		return true
	}

	return false
}

// createHarvester creates a new harvester instance from the given state
func (l *Log) createHarvester(state file.State) (*harvester.Harvester, error) {

	// Each harvester gets its own copy of the outlet
	outlet := l.outlet.Copy()
	h, err := harvester.NewHarvester(
		l.cfg,
		state,
		l.states,
		outlet,
	)

	return h, err
}

// startHarvester starts a new harvester with the given offset
// In case the HarvesterLimit is reached, an error is returned
func (l *Log) startHarvester(state file.State, offset int64) error {

	if l.config.HarvesterLimit > 0 && l.registry.len() >= l.config.HarvesterLimit {
		harvesterSkipped.Add(1)
		return fmt.Errorf("Harvester limit reached")
	}

	// Set state to "not" finished to indicate that a harvester is running
	state.Finished = false
	state.Offset = offset

	// Create harvester with state
	h, err := l.createHarvester(state)
	if err != nil {
		return err
	}

	reader, err := h.Setup()
	if err != nil {
		return fmt.Errorf("Error setting up harvester: %s", err)
	}

	l.registry.start(h, reader)

	return nil
}

// updateState updates the prospector state and forwards the event to the spooler
// All state updates done by the prospector itself are synchronous to make sure not states are overwritten
func (l *Log) updateState(state file.State) error {

	// Add ttl if cleanOlder is enabled and TTL is not already 0
	if l.config.CleanInactive > 0 && state.TTL != 0 {
		state.TTL = l.config.CleanInactive
	}

	// Update first internal state
	l.states.Update(state)

	data := util.NewData()
	data.SetState(state)

	ok := l.outlet.OnEvent(data)

	if !ok {
		logp.Info("Prospector outlet closed")
		return errors.New("prospector outlet closed")
	}

	return nil
}
