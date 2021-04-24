package f2

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/gookit/color"
	"github.com/urfave/cli/v2"
	"gopkg.in/djherbis/times.v1"
)

var (
	red    = color.HEX("#FF2F2F")
	green  = color.HEX("#23D160")
	yellow = color.HEX("#FFAB00")
)

var (
	errInvalidArgument = errors.New(
		"Invalid argument: one of `-f`, `-r` or `-u` must be present and set to a non empty string value\nUse 'f2 --help' for more information",
	)

	errConflictDetected = fmt.Errorf(
		"Conflict detected! Please resolve before proceeding or append the %s flag to fix conflicts automatically",
		yellow.Sprint("-F"),
	)
)

const (
	windows = "windows"
	darwin  = "darwin"
)

const (
	dotCharacter = 46
)

// Change represents a single filename change
type Change struct {
	BaseDir string `json:"base_dir"`
	Source  string `json:"source"`
	Target  string `json:"target"`
	IsDir   bool   `json:"is_dir"`
}

// renameError represents an error that occurs when
// renaming a file
type renameError struct {
	entry Change
	err   error
}

// Operation represents a batch renaming operation
type Operation struct {
	paths         []Change
	matches       []Change
	conflicts     map[conflict][]Conflict
	findString    string
	replacement   string
	startNumber   int
	exec          bool
	fixConflicts  bool
	includeHidden bool
	includeDir    bool
	onlyDir       bool
	ignoreCase    bool
	ignoreExt     bool
	searchRegex   *regexp.Regexp
	directories   []string
	recursive     bool
	undoFile      string
	outputFile    string
	workingDir    string
	stringMode    bool
	excludeFilter []string
	maxDepth      int
	sort          string
	reverseSort   bool
	quiet         bool
	errors        []renameError
}

type mapFile struct {
	Date       string   `json:"date"`
	Operations []Change `json:"operations"`
}

// writeToFile writes the details of a successful operation
// to the specified output file, creating it if necessary.
func (op *Operation) writeToFile(outputFile string) (err error) {
	// Create or truncate file
	file, err := os.Create(outputFile)
	if err != nil {
		return err
	}

	defer func() {
		ferr := file.Close()
		if ferr != nil {
			err = ferr
		}
	}()

	mf := mapFile{
		Date:       time.Now().Format(time.RFC3339),
		Operations: op.matches,
	}

	writer := bufio.NewWriter(file)
	b, err := json.MarshalIndent(mf, "", "    ")
	if err != nil {
		return err
	}
	_, err = writer.Write(b)
	if err != nil {
		return err
	}

	return writer.Flush()
}

// undo reverses a successful renaming operation indicated
// in the specified map file
func (op *Operation) undo() error {
	file, err := os.ReadFile(op.undoFile)
	if err != nil {
		return err
	}

	var mf mapFile
	err = json.Unmarshal(file, &mf)
	if err != nil {
		return err
	}
	op.matches = mf.Operations

	for i, v := range op.matches {
		ch := v
		ch.Source = v.Target
		ch.Target = v.Source

		op.matches[i] = ch
	}

	if !op.exec && op.sort != "" {
		err = op.sortBy()
		if err != nil {
			return err
		}
	}

	return op.apply()
}

// sortBySize sorts the matches according to their file size
func (op *Operation) sortBySize() (err error) {
	sort.SliceStable(op.matches, func(i, j int) bool {
		ipath := filepath.Join(op.matches[i].BaseDir, op.matches[i].Source)
		jpath := filepath.Join(op.matches[j].BaseDir, op.matches[j].Source)

		var ifile, jfile fs.FileInfo
		ifile, err = os.Stat(ipath)
		jfile, err = os.Stat(jpath)

		isize := ifile.Size()
		jsize := jfile.Size()

		if op.reverseSort {
			return isize < jsize
		}

		return isize > jsize
	})

	return err
}

// sortByTime sorts the matches by the specified file attribute
// (mtime, atime, btime or ctime)
func (op *Operation) sortByTime() (err error) {
	sort.SliceStable(op.matches, func(i, j int) bool {
		ipath := filepath.Join(op.matches[i].BaseDir, op.matches[i].Source)
		jpath := filepath.Join(op.matches[j].BaseDir, op.matches[j].Source)

		var ifile, jfile times.Timespec
		ifile, err = times.Stat(ipath)
		jfile, err = times.Stat(jpath)

		var itime, jtime time.Time
		switch op.sort {
		case modTime:
			itime = ifile.ModTime()
			jtime = jfile.ModTime()
		case birthTime:
			itime = ifile.ModTime()
			jtime = jfile.ModTime()
			if ifile.HasBirthTime() {
				itime = ifile.BirthTime()
			}
			if jfile.HasBirthTime() {
				jtime = jfile.BirthTime()
			}
		case accessTime:
			itime = ifile.AccessTime()
			jtime = jfile.AccessTime()
		case changeTime:
			itime = ifile.ModTime()
			jtime = jfile.ModTime()
			if ifile.HasChangeTime() {
				itime = ifile.ChangeTime()
			}
			if jfile.HasChangeTime() {
				jtime = jfile.ChangeTime()
			}
		}

		it, jt := itime.UnixNano(), jtime.UnixNano()

		if op.reverseSort {
			return it < jt
		}

		return it > jt
	})

	return err
}

// sortBy delegates the sorting of matches to the appropriate method
func (op *Operation) sortBy() (err error) {
	switch op.sort {
	case "size":
		return op.sortBySize()
	case accessTime, modTime, birthTime, changeTime:
		return op.sortByTime()
	default:
		return nil
	}
}

// printChanges displays the changes to be made in a
// table format
func (op *Operation) printChanges() {
	var data = make([][]string, len(op.matches))
	for i, v := range op.matches {
		source := filepath.Join(v.BaseDir, v.Source)
		target := filepath.Join(v.BaseDir, v.Target)
		d := []string{source, target, green.Sprint("ok")}
		data[i] = d
	}

	printTable(data)
}

// rename iterates over all the matches and renames them on the filesystem
// directories are auto-created if necessary.
// Errors are aggregated instead of being reported one by one
func (op *Operation) rename() {
	var errs []renameError
	for _, ch := range op.matches {
		var source, target = ch.Source, ch.Target
		source = filepath.Join(ch.BaseDir, source)
		target = filepath.Join(ch.BaseDir, target)

		renameErr := renameError{
			entry: ch,
		}

		// If target contains a slash, create all missing
		// directories before renaming the file
		if strings.Contains(ch.Target, "/") ||
			strings.Contains(ch.Target, `\`) && runtime.GOOS == windows {
			// No need to check if the `dir` exists or if there are several
			// consecutive slashes since `os.MkdirAll` handles that
			dir := filepath.Dir(ch.Target)
			err := os.MkdirAll(filepath.Join(ch.BaseDir, dir), 0750)
			if err != nil {
				renameErr.err = err
				errs = append(errs, renameErr)
				continue
			}
		}

		if err := os.Rename(source, target); err != nil {
			renameErr.err = err
			errs = append(errs, renameErr)
		}
	}

	op.errors = errs
}

// reportErrors displays the errors that occur during a renaming operation
func (op *Operation) reportErrors() {
	var data = make([][]string, len(op.errors)+len(op.matches))
	for i, v := range op.matches {
		source := filepath.Join(v.BaseDir, v.Source)
		target := filepath.Join(v.BaseDir, v.Target)
		d := []string{source, target, green.Sprint("success")}
		data[i] = d
	}

	for i, v := range op.errors {
		source := filepath.Join(v.entry.BaseDir, v.entry.Source)
		target := filepath.Join(v.entry.BaseDir, v.entry.Target)

		msg := v.err.Error()
		msg = strings.TrimSpace(msg[strings.IndexByte(msg, ':'):])
		d := []string{
			source,
			target,
			red.Sprintf("%s", strings.TrimPrefix(msg, ": ")),
		}
		data[i+len(op.matches)] = d
	}

	printTable(data)
}

// handleErrors is used to report the errors and write any successful
// operations to a file
func (op *Operation) handleErrors() error {
	// first remove the error entries from the matches so they are not confused
	// with successful operations
	for _, v := range op.errors {
		target := v.entry.Target
		for j := len(op.matches) - 1; j >= 0; j-- {
			if target == op.matches[j].Target {
				op.matches = append(op.matches[:j], op.matches[j+1:]...)
			}
		}
	}

	op.reportErrors()

	file := fmt.Sprintf(
		".f2_%s.json",
		time.Now().Format("2006-01-2T15-04-05PM"),
	)

	var err error
	if len(op.matches) > 0 {
		err = op.writeToFile(file)
	}

	if err == nil && len(op.matches) > 0 {
		return fmt.Errorf(
			"Some files could not be renamed. The successful operations have been written to %s. To revert the changes, pass this file to the %s flag",
			yellow.Sprint(file),
			yellow.Sprint("--undo"),
		)
	} else if err != nil && len(op.matches) > 0 {
		return fmt.Errorf("The above files could not be renamed")
	}

	return fmt.Errorf("The renaming operation failed due to the above errors")
}

// apply will check for conflicts and print the changes to be made
// or apply them directly to the filesystem if in execute mode.
// Conflicts will be ignored if indicated
func (op *Operation) apply() error {
	if len(op.matches) == 0 {
		if !op.quiet {
			fmt.Println("Failed to match any files")
		}
		return nil
	}

	op.validate()
	if len(op.conflicts) > 0 && !op.fixConflicts {
		if !op.quiet {
			op.reportConflicts()
		}

		return errConflictDetected
	}

	if op.exec {
		if op.includeDir || op.undoFile != "" {
			op.sortMatches()
		}

		op.rename()

		if len(op.errors) > 0 {
			return op.handleErrors()
		}

		if op.outputFile != "" {
			return op.writeToFile(op.outputFile)
		}
	} else {
		if op.quiet {
			return nil
		}
		op.printChanges()
		fmt.Printf("Append the %s flag to apply the above changes\n", yellow.Sprint("-x"))
	}

	return nil
}

// sortMatches is used to sort files to avoid renaming conflicts
func (op *Operation) sortMatches() {
	sort.SliceStable(op.matches, func(i, j int) bool {
		// sort parent directories before child directories in undo mode
		if op.undoFile != "" {
			return len(op.matches[i].BaseDir) < len(op.matches[j].BaseDir)
		}

		// sort files before directories
		if !op.matches[i].IsDir {
			return true
		}

		// sort child directories before parent directories
		return len(op.matches[i].BaseDir) > len(op.matches[j].BaseDir)
	})
}

func (op *Operation) replaceString(fileName string) (str string) {
	findString := op.findString
	if findString == "" {
		findString = fileName
	}
	replacement := op.replacement

	if strings.HasPrefix(replacement, `\C`) && len(replacement) == 3 {
		matches := op.searchRegex.FindAllString(fileName, -1)
		str = fileName
		for _, v := range matches {
			switch replacement {
			case `\Cu`:
				str = strings.ReplaceAll(str, v, strings.ToUpper(v))
			case `\Cl`:
				str = strings.ReplaceAll(str, v, strings.ToLower(v))
			case `\Ct`:
				str = strings.ReplaceAll(
					str,
					v,
					strings.Title(strings.ToLower(v)),
				)
			}
		}
		return
	}

	if op.stringMode {
		if op.ignoreCase {
			str = op.searchRegex.ReplaceAllString(fileName, replacement)
		} else {
			str = strings.ReplaceAll(fileName, findString, replacement)
		}
	} else {
		str = op.searchRegex.ReplaceAllString(fileName, replacement)
	}

	return str
}

// replace replaces the matched text in each path with the
// replacement string
func (op *Operation) replace() (err error) {
	for i, v := range op.matches {
		fileName, dir := filepath.Base(v.Source), filepath.Dir(v.Source)
		fileExt := filepath.Ext(fileName)
		if op.ignoreExt {
			fileName = filenameWithoutExtension(fileName)
		}

		str := op.replaceString(fileName)

		// handle variables
		str, err = op.handleVariables(str, v)
		if err != nil {
			return err
		}

		// If numbering scheme is present
		if indexRegex.Match([]byte(str)) {
			str, err = op.replaceIndex(str, i)
			if err != nil {
				return err
			}
		}

		if op.ignoreExt {
			str += fileExt
		}

		v.Target = filepath.Join(dir, str)
		op.matches[i] = v
	}

	return nil
}

// findMatches locates matches for the search pattern
// in each filename. Hidden files and directories are exempted
func (op *Operation) findMatches() error {
	for _, v := range op.paths {
		filename := filepath.Base(v.Source)

		if v.IsDir && !op.includeDir {
			continue
		}

		if op.onlyDir && !v.IsDir {
			continue
		}

		// ignore dotfiles on unix and hidden files on windows
		if !op.includeHidden {
			r, err := isHidden(filename, v.BaseDir)
			if err != nil {
				return err
			}
			if r {
				continue
			}
		}

		var f = filename
		if op.ignoreExt {
			f = filenameWithoutExtension(f)
		}

		if op.stringMode {
			findStr := op.findString

			if op.ignoreCase {
				f = strings.ToLower(f)
				findStr = strings.ToLower(findStr)
			}

			if strings.Contains(f, findStr) {
				op.matches = append(op.matches, v)
			}
			continue
		}

		matched := op.searchRegex.MatchString(f)
		if matched {
			op.matches = append(op.matches, v)
		}
	}

	return nil
}

// filterMatches excludes any files or directories that match
// the find pattern in accordance with the provided exclude pattern
func (op *Operation) filterMatches() error {
	var filtered []Change
	filters := strings.Join(op.excludeFilter, "|")
	regex, err := regexp.Compile(filters)
	if err != nil {
		return err
	}

	for _, m := range op.matches {
		if !regex.MatchString(m.Source) {
			filtered = append(filtered, m)
		}
	}

	op.matches = filtered
	return nil
}

// setPaths creates a Change struct for each path
// and checks if its a directory or not
func (op *Operation) setPaths(paths map[string][]os.DirEntry) {
	for k, v := range paths {
		for _, f := range v {
			var change = Change{
				BaseDir: k,
				IsDir:   f.IsDir(),
				Source:  filepath.Clean(f.Name()),
			}

			op.paths = append(op.paths, change)
		}
	}
}

// run executes the operation sequence
func (op *Operation) run() error {
	if op.undoFile != "" {
		return op.undo()
	}

	err := op.findMatches()
	if err != nil {
		return err
	}

	if len(op.excludeFilter) != 0 {
		err = op.filterMatches()
		if err != nil {
			return err
		}
	}

	if op.sort != "" {
		err = op.sortBy()
		if err != nil {
			return err
		}
	}

	err = op.replace()
	if err != nil {
		return err
	}

	return op.apply()
}

// setOptions applies the command line arguments
// onto the operation
func setOptions(op *Operation, c *cli.Context) error {
	op.outputFile = c.String("output-file")
	op.findString = c.String("find")
	op.replacement = c.String("replace")
	op.exec = c.Bool("exec")
	op.fixConflicts = c.Bool("fix-conflicts")
	op.includeDir = c.Bool("include-dir")
	op.includeHidden = c.Bool("hidden")
	op.ignoreCase = c.Bool("ignore-case")
	op.ignoreExt = c.Bool("ignore-ext")
	op.recursive = c.Bool("recursive")
	op.directories = c.Args().Slice()
	op.undoFile = c.String("undo")
	op.onlyDir = c.Bool("only-dir")
	op.stringMode = c.Bool("string-mode")
	op.excludeFilter = c.StringSlice("exclude")
	op.maxDepth = c.Int("max-depth")
	op.quiet = c.Bool("quiet")

	// Sorting
	if c.String("sort") != "" {
		op.sort = c.String("sort")
	} else if c.String("sortr") != "" {
		op.sort = c.String("sortr")
		op.reverseSort = true
	}

	if op.onlyDir {
		op.includeDir = true
	}

	findPattern := c.String("find")
	// Match entire string if find pattern is empty
	if findPattern == "" {
		findPattern = ".*"
	}

	if op.ignoreCase {
		findPattern = "(?i)" + findPattern
	}

	re, err := regexp.Compile(findPattern)
	if err != nil {
		return err
	}
	op.searchRegex = re

	return nil
}

// newOperation returns an Operation constructed
// from command line flags & arguments
func newOperation(c *cli.Context) (*Operation, error) {
	if c.String("find") == "" && c.String("replace") == "" &&
		c.String("undo") == "" {
		return nil, errInvalidArgument
	}

	op := &Operation{}
	err := setOptions(op, c)
	if err != nil {
		return nil, err
	}

	if op.undoFile != "" {
		return op, nil
	}

	var paths = make(map[string][]os.DirEntry)
	for _, v := range op.directories {
		paths[v], err = os.ReadDir(v)
		if err != nil {
			return nil, err
		}
	}

	// Use current directory
	if len(paths) == 0 {
		paths["."], err = os.ReadDir(".")
		if err != nil {
			return nil, err
		}
	}

	if op.recursive {
		paths, err = walk(paths, op.includeHidden, op.maxDepth)
		if err != nil {
			return nil, err
		}
	}

	// Get the current working directory
	op.workingDir, err = filepath.Abs(".")
	if err != nil {
		return nil, err
	}

	op.setPaths(paths)
	return op, nil
}
