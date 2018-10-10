package watcher

import (
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
	Op,
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

	stat, err := os.Stat(name)
	if err != nil {
		return nil, err
	}

	fileList[name] = stat
	if !stat.IsDir() {
		return fileList, nil
	}

	fInfoList, err := ioutil.ReadDir(name)
	if err != nil {
		return nil, err
	}

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



