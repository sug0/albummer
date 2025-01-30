package main

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/microcosm-cc/bluemonday"
	"gopkg.in/russross/blackfriday.v2"
)

var imgExtensions = map[string]int{".png": 1, ".jpg": 1, ".jpeg": 1}
var vidExtensions = map[string]int{".mp4": 1}
var wavExtensions = map[string]int{".wav": 1}

const mediaTypeImg = 0
const mediaTypeVid = 1
const mediaTypeWav = 2

type MediaFile struct {
	path      string
	mediaType int
	mtime     time.Time
	html      string
}

// We create a collection type MediaFiles, as array of MediaFile structs
// Then we implement the Sort interface: Len(), Swap(), Less() - to sort by
// mtime
type MediaFiles []MediaFile

func (m MediaFiles) Len() int {
	return len(m)
}

func (m MediaFiles) Swap(i, j int) {
	m[i], m[j] = m[j], m[i]
}

func (m MediaFiles) Less(i, j int) bool {
	return m[i].mtime.Before(m[j].mtime)
}

// turn list into map[basename] -> *MediaFile
func (m MediaFiles) ToMap() map[string]*MediaFile {
	ret := make(map[string]*MediaFile)

	for _, mf := range m {
		_, fn := filepath.Split(mf.path)
		p := new(MediaFile)
		*p = mf
		ret[fn] = p
	}
	return ret
}

func getExeFolder() string {
	exe, _ := os.Executable()
	path, _ := filepath.Split(exe)
	return path
}

func abort(msg string, exitCode int) {
	fmt.Println(msg)
	os.Exit(exitCode)
}

func help() {
	fmt.Printf(usage, os.Args[0])
	os.Exit(0)
}

func main() {
	//	defer profile.Start().Stop()
	if len(os.Args) == 1 {
		help()
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "make-template":
		makeTemplate(args)
	case "generate":
		generate(args)
	default:
		help()
	}
}

func getLowerExtension(path string) string {
	return filepath.Ext(strings.ToLower(path))
}

func makeTemplate(args []string) {
	if len(args) < 1 {
		abort("Please specify a media folder and an output filename", 1)
	}
	if len(args) < 2 {
		abort("Please specify an output filename", 1)
	}

	folder := args[0]
	outfile := args[1]
	css := filepath.Join(getExeFolder(), "default.css")
	numCols := 3
	order := "asc"

	if len(args) > 2 {
		n, err := strconv.Atoi(args[2])
		numCols = n
		if err != nil {
			numCols = 3
		}
	}

	if len(args) > 3 {
		order = args[3]
	}

	if len(args) > 4 {
		css = args[4]
	}

	allMedia, err := getAllMedia(folder)
	if order == "asc" {
		sort.Sort(MediaFiles(allMedia))
	} else {
		sort.Sort(sort.Reverse(MediaFiles(allMedia)))
	}
	if err != nil {
		abort(err.Error(), 1)
	}

	var mediaBody string
	var lineLen int

	for _, m := range allMedia {
		_, fn := filepath.Split(m.path)
		if m.mediaType == mediaTypeVid || m.mediaType == mediaTypeWav {
			if lineLen > 0 {
				mediaBody += "\n"
			}
			mediaBody += fmt.Sprintf("\n%s\n\n", fn)
			lineLen = 0
		} else {
			if lineLen > 0 {
				mediaBody += "   "
			}
			mediaBody += fn
			lineLen += 1
			if lineLen == numCols {
				mediaBody += "\n"
				lineLen = 0
			}
		}
	}

	absFolder, err := filepath.Abs(folder)
	_, title := filepath.Split(absFolder)

	f, err := os.Create(outfile)
	if err != nil {
		panic(err)
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	_, err = w.WriteString(fmt.Sprintf(":folder %s\n:show_filenames\n:use %s\n\n# %s\n\n%s\n", folder, css, title, mediaBody))
	if err != nil {
		panic(err)
	}
	w.Flush()
	fmt.Println("Generated", outfile)
}

func parseFolder(lines []string) (string, error) {
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		if line[0] == ':' {
			// we have a control line
			cols := strings.Fields(line)
			switch cols[0] {
			case ":folder":
				folder := cols[1]
				return folder, nil
			}
		}
	}
	return "", errors.New("No folder in album file")
}

func loadMedia(lines []string, folder string, allMedia *map[string]*MediaFile) {
	c := make(chan int)
	numMedia := 0
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		if line[0] == ':' {
			continue
		} else {
			// we have a media or markdown line
			cols := strings.Fields(line)
			if len(cols) == 0 {
				continue
			}
			if _, ok := (*allMedia)[cols[0]]; ok {
				// we have a media line
				for _, col := range cols {
					if mediaFile, ok := (*allMedia)[col]; ok {
						switch mediaFile.mediaType {
						case mediaTypeImg:
							go func(mediaFile *MediaFile, col string, c chan int) {
								mediaFile.html = imgToHtml(folder, col)
								c <- 1
							}(mediaFile, col, c)
							numMedia++
						case mediaTypeVid:
							go func(mediaFile *MediaFile, col string, c chan int) {
								mediaFile.html = vidToHtml(folder, col)
								c <- 1
							}(mediaFile, col, c)
							numMedia++
						case mediaTypeWav:
							go func(mediaFile *MediaFile, col string, c chan int) {
								mediaFile.html = wavToHtml(folder, col)
								c <- 1
							}(mediaFile, col, c)
							numMedia++
						}
					}
				}
			}
		}
	}

	for i := 0; i < numMedia; i++ {
		fmt.Print(fmt.Sprintf("\r  Loading image / video %4d of %-4d ", i+1, numMedia))
		// wait for completion
		_ = <-c
	}
}

func generate(args []string) {
	if len(args) < 1 {
		abort("Please specify an input file!", 1)
	}

	inputFile := args[0]
	f, err := os.Open(inputFile)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)

	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		panic(err)
	}

	var folder string
	var css string
	var allMedia map[string]*MediaFile

	var htmlBodies []string
	var htmlHead string

	lc := 0
	lcMax := len(lines)

	folder, err = parseFolder(lines)
	if err != nil {
		abort("No folder in album file!", 1)
	}

	allMediaList, err := getAllMedia(folder)
	if err != nil {
		panic(err)
	}
	allMedia = allMediaList.ToMap()

	fmt.Println("The Albummer is processing", inputFile)
	loadMedia(lines, folder, &allMedia)
	fmt.Println()

	for lc < lcMax {
		line := lines[lc]
		lc += 1

		fmt.Print(fmt.Sprintf("\r  Generating for line   %4d of %-4d ", lc, lcMax))
		if len(line) == 0 {
			continue
		}
		if line[0] == ':' {
			// we have a control line
			cols := strings.Fields(line)
			switch cols[0] {
			case ":show_filenames":
				// show_filenames = true
			case ":use":
				css = cols[1]
				cssText, err := ioutil.ReadFile(css)
				if err == nil {
					htmlHead = fmt.Sprintf("<style>%s</style>",
						string(cssText))
				}
			} // end switch
		} else {
			// we have a media or markdown line
			cols := strings.Fields(line)
			if len(cols) == 0 {
				continue
			}
			if _, ok := allMedia[cols[0]]; ok {
				// we have a media line
				numCols := len(cols)
				percent := int(100 / numCols)
				html := `<div align="center"><table><tr>`
				for _, col := range cols {
					html += fmt.Sprintf(`<td style="width:%d%%;">`, percent)
					if mediaFile, ok := allMedia[col]; ok {
						html += mediaFile.html
						html += `</td><td width="10px"></td>`
					}
				}
				html += `</tr></table></div>`
				htmlBodies = append(htmlBodies, html)
			} else {
				// markdown block
				markdownLines := line
				for lc < lcMax {
					line = lines[lc]
					lc += 1
					if len(line) == 0 {
						markdownLines += "\n" + line
						continue
					}
					cols = strings.Fields(line)
					if _, ok := allMedia[cols[0]]; ok {
						// we have a media line -> end of markdown, put it back
						lc -= 1
						break
					}
					markdownLines += "\n" + line
				}
				unsafe := blackfriday.Run([]byte(markdownLines))
				html := bluemonday.UGCPolicy().SanitizeBytes(unsafe)
				htmlBodies = append(htmlBodies, string(html))
			}
		}
	}
	fmt.Println()

	ext := filepath.Ext(inputFile)
	outFile := strings.Replace(inputFile, ext, ".html", 1)
	of, err := os.Create(outFile)
	if err != nil {
		panic(err)
	}
	defer fmt.Println("Generated", outFile, "                ")
	defer of.Close()

	w := bufio.NewWriter(of)
	_, err = w.WriteString(fmt.Sprintf("<!DOCTYPE html><html><head><meta charset=\"UTF-8\">%s</head>\n<body>", htmlHead))
	if err != nil {
		panic(err)
	}
	numBodies := len(htmlBodies)
	for index, htmlBody := range htmlBodies {
		fmt.Print(fmt.Sprintf("\r  Writing HTML body     %4d of %-4d ", index+1, numBodies))
		_, err = w.WriteString(htmlBody)
		if err != nil {
			panic(err)
		}
	}
	fmt.Println()
	_, err = w.WriteString("</body>\n</html>")
	if err != nil {
		panic(err)
	}
	w.Flush()
	fmt.Print("   (closing file ...)\r")
}

func getAllMedia(root string) (MediaFiles, error) {
	var files MediaFiles

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if !info.IsDir() {
			ext := getLowerExtension(path)
			_, isImg := imgExtensions[ext]
			_, isVid := vidExtensions[ext]
			_, isWav := wavExtensions[ext]

			var mediaType int = mediaTypeImg
			if isVid {
				mediaType = mediaTypeVid
			} else if isWav {
				mediaType = mediaTypeWav
			}
			if isImg || isVid || isWav {
				files = append(files, MediaFile{path, mediaType, info.ModTime(), ""})
			}
		}
		return nil
	})
	return files, err
}

func imgToHtml(folder string, img string) string {
	data, err := ioutil.ReadFile(filepath.Join(folder, img))
	if err != nil {
		return ""
	}
	ext := filepath.Ext(strings.ToLower(img))
	var imgFormat string
	if ext == ".png" {
		imgFormat = "png"
	} else {
		imgFormat = "jpeg"
	}
	return fmt.Sprintf(`<div class="imgdiv"><img class="center-fit" src="data:image/%s;base64,%s"></img></div>`, imgFormat, base64.StdEncoding.EncodeToString(data))
}

func vidToHtml(folder string, vid string) string {
	data, err := ioutil.ReadFile(filepath.Join(folder, vid))
	if err != nil {
		return ""
	}
	return fmt.Sprintf(`<div class="viddiv"><video class="center-fit" controls src="data:video/mp4;base64,%s"></video></div>`, base64.StdEncoding.EncodeToString(data))
}

func wavToHtml(folder string, vid string) string {
	data, err := ioutil.ReadFile(filepath.Join(folder, vid))
	if err != nil {
		return ""
	}
	return fmt.Sprintf(`<div align="center"><audio controls src="data:audio/x-wav;base64,%s"></audio></div>`, base64.StdEncoding.EncodeToString(data))
}

var usage = `Usage: %s command options 
Where command can be:
  make-template media_folder output.alb [num_cols] [order] [custom.css]
    This will create the album file, ready for editing, as the first step 
    of creating an HTML album.

    Arguments:
    - media_folder : the folder containing images and videos
    - output.alb   : the album file to be generated
    - num_cols     : optional, default=3. The number of columns to use when 
                     laying out images.  Videos will always be placed on a 
                     separate line.
    - order        : optional, default=asc : Sort order of the media, by file 
                     timestamp. If you specify anything other than asc, then 
                     descending order (newest first) will be used.
    - custom.css   : optional, default=default.css : for pros: specify your 
                     custom CSS file
   
  generate album_file
    Generates the single-file HTML from an album file, with extension .html

    Arguments:
    - album_file   : the album file to be converted. If album_file is 
                     my_fotos.alb, the generated HTML file will be named 
                     my_fotos.html
`
