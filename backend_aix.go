//go:build aix

package fsnotify

import (
		"errors"
		"io/ioutil"
		"os"
		"path/filepath"
		"sync"
		"time"
)

const (
		sleepTime time.Duration = 100 * time.Millisecond
)

type aix_watcher struct {
		*shared
		Events chan Event
		Errors chan error
		mu       *sync.Mutex // Map access
		isClosed bool
		watches map[string]bool        // whatched files (recursive or not)
		files   map[string]os.FileInfo // map of files.
		maxEvents int
		close     chan struct{}
		done   chan struct{}
}

var defaultBufferSize = 0

func newBackend(ev chan Event, errs chan error) (backend, error) {
        w := &aix_watcher {
                shared:  newShared(ev, errs),
                Events:  ev,
                Errors:  errs,
                close:   make(chan struct{}),
                mu:      new(sync.Mutex),
                files:   make(map[string]os.FileInfo),
                watches: make(map[string]bool),
        }
        go w.readEvents()
        return w, nil
}

func (w *aix_watcher) listRecursive(name string) (map[string]os.FileInfo, error) {
        fileList := make(map[string]os.FileInfo)

        return fileList, filepath.Walk(name, func(path string, info os.FileInfo, err error) error {
                if err != nil {
                        return err
                }
                // Add the path and it's info to the file list.
                fileList[path] = info
                return nil
        })
}

func (w *aix_watcher) list(name string) (map[string]os.FileInfo, error) {
        fileList := make(map[string]os.FileInfo)

        // Make sure name exists.
        stat, err := os.Stat(name)
        if err != nil {
                return nil, err
        }

        fileList[name] = stat

        // If it's not a directory, just return.
        if !stat.IsDir() {
                return fileList, nil
         }

         // It's a directory.
         fInfoList, err := ioutil.ReadDir(name)
         if err != nil {
                return nil, err
         }
         // Add all of the files in the directory to the file list.
         for _, fInfo := range fInfoList {
                 path := filepath.Join(name, fInfo.Name())
                 fileList[path] = fInfo
         }
         return fileList, nil
}

func (w *aix_watcher) retrieveFileList() map[string]os.FileInfo {
		w.mu.Lock()
		defer w.mu.Unlock()

		fileList := make(map[string]os.FileInfo)

		var list map[string]os.FileInfo
		var err error

		for name, recursive := range w.watches {
				if recursive {
						list, err = w.listRecursive(name)
						if err != nil {
								if os.IsNotExist(err) {
								// A watch file can be removed
										continue
						} else {
								w.Errors <- err
						}
				}
				} else {
						list, err = w.list(name)
						if err != nil {
								if os.IsNotExist(err) {
										// A watch file can be removed
										continue
								} else {
										w.Errors <- err
								}
						}
				}
				// Add the file's to the file list.
				for k, v := range list {
						fileList[k] = v
				}
		}

		return fileList
}

func (w *aix_watcher) pollEvents(files map[string]os.FileInfo, evt chan Event,
		cancel chan struct{}) {
		w.mu.Lock()
		defer w.mu.Unlock()

		// Store create and remove events for use to check for rename events.
		creates := make(map[string]os.FileInfo)
		removes := make(map[string]os.FileInfo)

		// Check for removed files.
		for path, info := range w.files {
				if _, found := files[path]; !found {
						removes[path] = info
						w.sendEvent(Event{Name: path, Op: Remove})
				}
		}

		// Check for created files, writes and chmods.
		for path, info := range files {
				oldInfo, found := w.files[path]
				if !found {
						// A file was created.
						creates[path] = info
						w.sendEvent(Event{Name: path, Op: Create})
						continue
				}
				if oldInfo.ModTime() != info.ModTime() {
						if info.IsDir() {
								// Modification on the directory means a file
								// have been changed (add, remove, etc),
								// but not a write event.
								continue
						}
						select {
								case <-cancel:
										return
								case evt <- Event{path, Write, ""}:
										w.sendEvent(Event{Name: path, Op: Write})
						}
				}
				if oldInfo.Mode() != info.Mode() {
						select {
								case <-cancel:
										return
								case evt <- Event{path, Chmod, ""}:
						}
				}
		}

		// Check for renames
		for path1, info1 := range removes {
				for _, info2 := range creates {
						// In some case, a newly created file can have the same
						// inode number than a deleted one.
						// As os.SameFile is only checking that it might not
						// be fully accurate. Checking that both files have the
						// same type allows to be a bit more accurate.
						if os.SameFile(info1, info2) && info1.IsDir() == info2.IsDir() {
								e := Event{
										Op:   Rename,
										Name: path1,
								}

								// Do not delete path2 from creates, as both events
								// are needed:
								// - path1, Rename
								// - path2, Create
								delete(removes, path1)

								select {
										case <-cancel:
										return
										case evt <- e:
								}
						}
				}
		}

		// Send all the remaining create and remove events.
		for path, _ := range creates {
				select {
						case <-cancel:
						return
						case evt <- Event{path, Create, ""}:
				}
		}
		for path, _ := range removes {
				select {
						case <-cancel:
								return
						case evt <- Event{path, Remove, ""}:
				}
		}
}

func (w *aix_watcher) readEvents() {
		outer:
		for {
					// done lets the inner polling cycle loop know when the
					// current cycle's method has finished executing.
					done := make(chan struct{})

					// Any events that are found are first piped to evt before
					// being sent to the main Event channel.
					evt := make(chan Event)

					// Retrieve the file list for all watched file's and dirs.
					fileList := w.retrieveFileList()

					// cancel can be used to cancel the current event polling
					//function.
					cancel := make(chan struct{})

					// Look for events.
					go func() {
							w.pollEvents(fileList, evt, cancel)
							done <- struct{}{}
					}()
					inner:
					for {
							select {
									case <-w.close:
										close(cancel)
										select {
												case <-done:
										}
										break outer
										case event := <-evt:
										select {
												case w.Events <- event:
												case <-w.close:
														close(cancel)
														break outer
												default:
														continue
										}
										case <-done: // Current cycle is finished.
												break inner
							}
					}

					// Update the file's list.
					w.mu.Lock()
					w.files = fileList
					w.mu.Unlock()

					// Sleep and then continue to the next loop iteration.
					time.Sleep(sleepTime)
		}

		close(w.Events)
		close(w.Errors)

}

// Add starts watching the named file or directory (non-recursively).
func (w *aix_watcher) Add(name string) error {
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.isClosed {
				return errors.New("watcher instance already closed")
		}

		name, err := filepath.Abs(name)
		if err != nil {
				return err
		}

		// Add the directory's contents to the files list.
		fileList, err := w.list(name)
		if err != nil {
				return err
		}
		for k, v := range fileList {
				w.files[k] = v
		}

		// Add the name to the watches list.
		w.watches[name] = false

		return nil
}

func (w *aix_watcher) AddWith(name string, opts ...addOpt) error {
		return nil
} 

// Close removes all watches and closes the events channel.
func (w *aix_watcher) Close() error {
		w.mu.Lock()
		if w.isClosed {
				w.mu.Unlock()
				return nil
		}
		w.isClosed = true
		w.files = make(map[string]os.FileInfo)
		w.watches = make(map[string]bool)
		w.mu.Unlock()

		// Send a close signal to the readEvents method.
		w.close <- struct{}{}
		return nil
}

func (w *aix_watcher) WatchList() []string { return nil }

// Remove stops watching the the named file or directory (non-recursively).
func (w *aix_watcher) Remove(name string) error {
		w.mu.Lock()
		defer w.mu.Unlock()

		name, err := filepath.Abs(name)
		if err != nil {
				return err

		}

		// Remove the name from w's watches list.
		delete(w.watches, name)

		// If name is a single file, remove it and return.
		info, found := w.files[name]
		if !found {
				return nil // Doesn't exist, just return.
		}
		if !info.IsDir() {
				delete(w.files, name)
				return nil
		}

		// Delete the actual directory from w.files
		delete(w.files, name)

		// If it's a directory, delete all of it's contents from w.files.
		for path := range w.files {
				if filepath.Dir(path) == name {
						delete(w.files, path)
				}
		}
		return nil
}

func (w *aix_watcher) xSupports(op Op) bool { return true }
