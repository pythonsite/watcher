package watcher

import (
	"time"
	"strings"
	"path/filepath"
	"errors"
	"fmt"
	"os"
	"sync"
	"io/ioutil"
)

var (
	// 当调用watcher的start方法的事件小于1纳秒的时候会提示这个错误
	ErrDurationTooShort = errors.New("error:duration is less than 1ns")
	// 如果已经调用了watcher的start方法，并且轮询已经开始，再次调用start方法提示这个错误
	ErrWatcherRunning = errors.New("error:watcher is already running")
	// 如果被监控的文件或目录已经被删除了，提示这个错误
	ErrWatchedFileDeleted = errors.New("error: watched file or folder deleted")
)

// 从这里到String方法之间的代码方式可以学习学习这种风格
type Op uint32

const (
	Create Op = iota
	Write
	Remove
	Rename
	Chmod
	Move
)

var ops = map[Op]string{
	Create: "CREATE",
	Write:  "WRITE",
	Remove: "REMOVE",
	Rename: "RENAME",
	Chmod:  "CHMOD",
	Move:   "MOVE",
}

func (e Op) String() string {
	if op, found := ops[e]; found {
		return op
	}
	return "???"
}

type Event struct {
	Op
	Path string
	os.FileInfo
}

func (e Event) String() string {
	if e.FileInfo != nil {
		pathType := "FILE"
		if e.IsDir() {
			pathType = "DIRECTORY"
		}
		return fmt.Sprintf("%s %q %s [%s]", pathType, e.Name(), e.Op, e.Path)
	}
	return "???"
}

// 这个是核心的结构体
type Watcher struct {
	Event  chan Event
	Error  chan error
	Closed chan struct{}
	close  chan struct{}
	wg     *sync.WaitGroup

	mu           *sync.Mutex
	runnning     bool
	names        map[string]bool
	files        map[string]os.FileInfo
	ignored      map[string]struct{}		// 要被忽略的文件或目录
	ops          map[Op]struct{}
	ignoreHidden bool						// 是否忽略隐藏文件
	maxEvents    int
}

// 用于初始化Watcher
func New() *Watcher {
	var wg sync.WaitGroup
	wg.Add(1)

	return &Watcher{
		Event:   make(chan Event),
		Error:   make(chan error),
		Closed:  make(chan struct{}),
		close:   make(chan struct{}),
		mu:      new(sync.Mutex),
		wg:      &wg,
		files:   make(map[string]os.FileInfo),
		ignored: make(map[string]struct{}),
		names:   make(map[string]bool),
	}
}

func (w *Watcher) SetMaxEvents(delta int) {
	w.mu.Lock()
	w.maxEvents = delta
	w.mu.Unlock()
}

// 设置是否忽略隐藏的文件或目录
func (w *Watcher) IgnoreHiddenFiles(ignore bool) {
	w.mu.Lock()
	w.ignoreHidden = ignore
	w.mu.Unlock()
}

// 设置自己需要过滤的事件
func (w *Watcher) FilterOps(ops ...Op) {
	w.mu.Lock()
	w.ops = make(map[Op]struct{})
	for _, op := range ops {
		w.ops[op] = struct{}{}
	}
	w.mu.Unlock()
}

// 添加一个单独文件或者一个目录到file list
func (w *Watcher) Add(name string) (err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	name, err = filepath.Abs(name)
	if err != nil {
		return err
	}

	// 如果文件在要忽略的list
	_, ignored := w.ignored[name]
	if ignored || (w.ignoreHidden && strings.HasPrefix(name, ".")) {
		return nil
	}
	fileList, err := w.list(name)
	if err != nil {
		return err
	}
	for k,v := range fileList {
		w.files[k] = v
	}
	w.names[name] = false
	return nil
}

func (w *Watcher) list(name string) (map[string]os.FileInfo, error) {
	fileList := make(map[string]os.FileInfo)

	// 确认文件是否存在
	stat, err := os.Stat(name)
	if err != nil {
		return nil, err
	}

	fileList[name] = stat
	// 如果不是一个目录的话直接返回
	if !stat.IsDir() {
		return fileList, nil
	}
	// 如果是一个目录按照下面处理
	fInfoList, err := ioutil.ReadDir(name)
	if err != nil {
		return nil, err
	}

	// 循环将在这个目录下的所有文件添加到 file list,当然这些文件不能是在要忽略的列表或者ignoreHidden设置为true
	for _, fInfo := range fInfoList {
		path := filepath.Join(name, fInfo.Name())
		_, ignored := w.ignored[path]
		if ignored || (w.ignoreHidden && strings.HasPrefix(fInfo.Name(), ".")) {
			continue
		}
		fileList[path] = fInfo
	}
	return fileList, nil
}

// 递归添加一个文件或者目录下的文件到file list
func (w *Watcher) AddRecursive(name string) (err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	name, err = filepath.Abs(name)
	if err != nil {
		return err
	}

	fileList, err := w.listRecursive(name)
	if err != nil {
		return err
	}
	for k, v := range fileList {
		w.files[k] = v
	}

	w.names[name] = true
	return nil
}

func (w *Watcher) listRecursive(name string) (map[string]os.FileInfo, error) {
	fileList := make(map[string]os.FileInfo)

	return fileList, filepath.Walk(name,func (path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		_, ignored := w.ignored[path]
		if ignored || (w.ignoreHidden && strings.HasPrefix(info.Name(), ".")) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		fileList[path] = info
		return nil
	})
}

// 从file list 中删除一个文件或者目录
func (w *Watcher) Remove(name string) (err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	name, err = filepath.Abs(name)
	if err != nil {
		return err
	}

	// 从w.names中删除一个name
	delete(w.names, name)

	// 如果name 是一个文件，则从files中删除
	info, found := w.files[name]
	if !found {
		return nil
	}
	if !info.IsDir() {
		delete(w.files, name)
		return nil
	}

	// 删除目录从w.files中
	delete(w.files, name)

	// 如果是一个目录则删除它包含的所有内容从files中
	for path := range w.files {
		if filepath.Dir(path) == name {
			delete(w.files, path)
		}
	}
	return nil
}

// 从文件列表中递归删除单个文件或者目录
func (w *Watcher) RemoveRecursive(name string) (err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	name, err = filepath.Abs(name)
	if err!= nil {
		return err
	}
	// 从names list中删除指定name
	delete(w.names, name)

	// 如果name是一个单个文件，删除它并且return
	info, found := w.files[name]
	if !found {
		return nil
	}

	if !info.IsDir() {
		delete(w.files,name)
		return nil
	}
	// 如果是一个目录， 删除所有的以及递归删除它包含的从w.files
	for path := range w.files {
		if strings.HasPrefix(path, name) {
			delete(w.files, path)
		}
	}
	return nil

}

// 添加要忽略的路径
// 将已经添加到files中的，忽略移除他们
func (w *Watcher) Ignore(paths ...string) (err error) {
	for _, path := range paths {
		path, err = filepath.Abs(path)
		if err != nil {
			return err
		}
		// 地推的删除所有我们添加的
		if err := w.RemoveRecursive(path); err != nil {
			return err
		}
		w.mu.Lock()
		w.ignored[path] = struct{}{}
		w.mu.Unlock()
	}
	return nil
}

// 返回一个files map 
func (w *Watcher) WatchedFiles() map[string]os.FileInfo {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.files
}

type fileInfo struct {
	name 		string
	size 		int64
	mode 		os.FileMode
	modTime	 	time.Time
	sys			interface{}
	dir 		bool
}

func (fs *fileInfo) IsDir() bool {
	return fs.dir
}

func (fs *fileInfo) ModTime() time.Time {
	return fs.modTime
}

func (fs *fileInfo) Mode() os.FileMode {
	return fs.mode
}

func (fs *fileInfo) Name() string {
	return fs.name
}

func (fs *fileInfo) Size() int64 {
	return fs.size
}

func (fs *fileInfo) Sys() interface{} {
	return fs.sys
}

// TriggerEvent 是一个用来触发事件的方法，与文件watching 进程是分开的
func (w *Watcher) TriggerEvent(eventType Op, file os.FileInfo) {
	w.Wait()
	if file == nil {
		file = &fileInfo{name: "triggered event", modTime: time.Now()}
	}
	w.Event <- Event{Op: eventType, Path: "-", FileInfo: file}
}

func(w *Watcher) retrieveFileList() map[string]os.FileInfo {
	w.mu.Lock()
	defer w.mu.Unlock()
	fileList := make(map[string]os.FileInfo)
	var list map[string]os.FileInfo
	var err error
	for name, recursive := range w.names {
		if recursive {
			list , err = w.listRecursive(name)
			if err != nil {
				if os.IsNotExist(err) {
					w.Error <- ErrWatchedFileDeleted
					w.mu.Unlock()
					w.RemoveRecursive(name)
					w.mu.Lock()
				} else {
					w.Error <- err
				}
			}
		} else {
			list ,err = w.list(name)
			if err != nil {
				if os.IsNotExist(err) {
					w.Error <- ErrWatchedFileDeleted
					w.mu.Unlock()
					w.Remove(name)
					w.mu.Lock()

				} else {
					w.Error <- err
				}
			}
		}
		for k,v := range list {
			fileList[k] = v
		}
	}
	return fileList
}

func (w *Watcher) Start(d time.Duration) error {
	if d < time.Nanosecond {
		return ErrDurationTooShort
	}
	w.mu.Lock()
	if w.runnning {
		w.mu.Unlock()
		return ErrWatcherRunning
	}
	w.runnning = true
	w.mu.Unlock()
	w.wg.Done()

	for {
		done := make(chan struct{})

		evt := make(chan Event)

		fileList := w.retrieveFileList()

		cancel := make(chan struct{})

		go func() {
			done <- struct{}{}
		}()
		
		numEvents := 0
	inner:
		for {
			select {
			case <- w.close:
				close(cancel)
				close(w.Closed)
				return nil
			case event := <-evt:
				if len(w.ops) >0 {
					_, found := w.ops[event.Op]
					if !found {
						continue
					}
				}
				numEvents++
				if w.maxEvents >0 && numEvents > w.maxEvents {
					close(cancel)
					break inner
				}
				w.Event <- event
			case <- done:
				break inner
			}

		}
		w.mu.Lock()
		w.files = fileList
		w.mu.Unlock()

		time.Sleep(d)
	}
}

func (w *Watcher) pollEvents(files map[string]os.FileInfo, evt chan Event,cancel chan struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()

	creates := make(map[string]os.FileInfo)
	removes := make(map[string]os.FileInfo)

	for path, info := range w.files {
		if _, found := files[path]; !found {
			removes[path] = info
		}
	}

	for path, info := range files {
		oldInfo, found := w.files[path]
		if !found {
			creates[path] = info
			continue
		}
		if oldInfo.ModTime() != info.ModTime() {
			select {
			case <- cancel:
				return
			case evt <- Event{Write, path, info}:

			}
		}

		if oldInfo.Mode() != info.Mode() {
			select {
			case <- cancel:
				return
			case evt <- Event{Chmod, path, info}:
			}
		}
	}
	for path1, info1 := range removes {
		for path2, info2 := range creates {
			if sameFile(info1, info2) {
				e := Event{
					Op:		Move,
					Path:	fmt.Sprintf("%s -> %s", path1, path2),
					FileInfo: info1,
				}
				if filepath.Dir(path1) == filepath.Dir(path2) {
					e.Op = Rename
				}
				delete(removes, path1)
				delete(creates, path2)

				select {
				case <- cancel:
					return
				case evt <- e:

				}
			}
		}
	}

	for path, info := range creates {
		select {
		case <- cancel:
			return
		case evt <- Event{Create, path, info}:

		}
	}

	for path, info := range removes {
		select {
		case <- cancel:
			return
		case evt <- Event{Remove, path, info}:
		}
	}

}

func (w *Watcher) Wait() {
	w.wg.Wait()
}

func (w *Watcher) Close() {
	w.mu.Lock()
	if !w.runnning {
		w.mu.Unlock()
		return
	}
	w.runnning = false
	w.files = make(map[string]os.FileInfo)
	w.names = make(map[string]bool)
	w.mu.Unlock()

	w.close <- struct{}{}
}





