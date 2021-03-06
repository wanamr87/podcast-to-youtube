// Copyright 2016 Google Inc. All rights reserved.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to writing, software distributed
// under the License is distributed on a "AS IS" BASIS, WITHOUT WARRANTIES OR
// CONDITIONS OF ANY KIND, either express or implied.
//
// See the License for the specific language governing permissions and
// limitations under the License.

// Command podcast-to-youtube generates videos using ffmpeg from any given
// podcast, by downloading the mp3 and adding a fixed image with a given logo
// and text.
package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	stdimage "image"
	"image/color"
	"image/png"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/oauth2/google"

	"google.golang.org/api/youtube/v3"

	"github.com/campoy/podcast-to-youtube/image"
	"github.com/campoy/tools/flags"
	"github.com/microcosm-cc/bluemonday"
)

var (
	rssFeed    = flag.String("rss", "http://feeds.feedburner.com/GcpPodcast?format=xml", "URL for the RSS feed")
	logo       = flag.String("logo", "resources/logo.png", "Path to the logo image. Supports PNG, GIF, and JPEG")
	font       = flag.String("font", "resources/Roboto-Light.ttf", "Font to be used in the video")
	titleTmpl  = flags.TextTemplate("title", "{{.Title}}: GCPPodcast {{.Number}}", "Template used for the title")
	foreground = flags.HexColor("fg", color.White, "Hex encoded color for the video text")
	background = flags.HexColor("bg", color.RGBA{0, 150, 136, 255}, "Hex encoded color for the video background")
	width      = flag.Int("w", 1280, "Width of the generated video in pixels")
	height     = flag.Int("h", 720, "Height of the generated video in pixels")
	tags       = flag.String("tags", "podcast,gcppodcast", "Comma separated list of tags to use in the YouTube upload")
)

func main() {
	flag.Parse()

	eps, err := fetchFeed(*rssFeed)
	if err != nil {
		failf("%v\n", err)
	}

	fmt.Print("episode number to publish (try 1, or 2-10): ")
	var answer string
	fmt.Scanln(&answer)
	from, to, err := parseRange(answer)
	if err != nil {
		failf("%s is an invalid range\n", answer)
	}

	var selected []episode
	for _, e := range eps {
		if from <= e.Number && e.Number <= to {
			selected = append(selected, e)
			fmt.Printf("episode %d: %s\n", e.Number, e.Title)
		}
	}
	if len(selected) == 0 {
		failf("no episodes selected\n")
	}

	fmt.Print("publish? (Y/n): ")
	answer = ""
	fmt.Scanln(&answer)
	if !(answer == "Y" || answer == "y" || answer == "") {
		return
	}

	client, err := authedClient()
	if err != nil {
		failf("could not authenticate with YouTube: %v\n", err)
	}

	for _, ep := range selected {
		if err := process(client, ep); err != nil {
			failf("episode %d: %v\n", ep.Number, err)
		}
	}
}

func failf(s string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, s, args...)
	os.Exit(1)
}

type episode struct {
	Title  string
	Number int
	Link   string
	Desc   string
	MP3    string
	Tags   []string
}

func fetchFeed(rss string) ([]episode, error) {
	res, err := http.Get(rss)
	if err != nil {
		return nil, fmt.Errorf("could not get %s: %v", rss, err)
	}
	defer res.Body.Close()

	var data struct {
		XMLName xml.Name `xml:"rss"`
		Channel []struct {
			Item []struct {
				Title  string `xml:"title"`
				Number int    `xml:"order"`
				Link   string `xml:"guid"`
				Desc   string `xml:"summary"`
				MP3    struct {
					URL string `xml:"url,attr"`
				} `xml:"enclosure"`
				Category []string `xml:"category"`
			} `xml:"item"`
		} `xml:"channel"`
	}

	if err := xml.NewDecoder(res.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("could not decode feed: %v", err)
	}

	var eps []episode
	for _, i := range data.Channel[0].Item {
		eps = append(eps, episode{
			Title:  i.Title,
			Number: i.Number,
			Link:   i.Link,
			Desc:   i.Desc,
			MP3:    i.MP3.URL,
			Tags:   i.Category,
		})
	}
	return eps, nil
}

// parseRange parses either a range (n-m) or a single episode number (n)
// and returns the first and last elements of the range.
func parseRange(s string) (first, last int, err error) {
	switch ps := strings.Split(s, "-"); len(ps) {
	case 1:
		n, err := strconv.Atoi(ps[0])
		return n, n, err
	case 2:
		from, err := strconv.Atoi(ps[0])
		if err != nil {
			return 0, 0, err
		}
		to, err := strconv.Atoi(ps[1])
		return from, to, err
	default:
		return 0, 0, errors.New("only formats supported are n or m-n")
	}
}

// authedClient performs an offline OAuth flow.
func authedClient() (*http.Client, error) {
	const path = "client_secrets.json"
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not open %s: %v", path, err)
	}
	cfg, err := google.ConfigFromJSON(b, youtube.YoutubeUploadScope)
	if err != nil {
		return nil, fmt.Errorf("could not parse config: %v", err)
	}

	url := cfg.AuthCodeURL("")
	fmt.Printf("Go here: \n\t%s\n", url)
	fmt.Printf("Then enter the code: ")
	var code string
	fmt.Scanln(&code)
	ctx := context.Background()
	tok, err := cfg.Exchange(ctx, code)
	if err != nil {
		return nil, err
	}
	return cfg.Client(ctx, tok), nil
}

// process creates the video for the given episode and uploads it
// to YouTube using an authenticated HTTP client.
func process(client *http.Client, ep episode) error {
	tmpDir, err := ioutil.TempDir("", "")
	if err != nil {
		return fmt.Errorf("could not create temp directory: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(tmpDir); err != nil {
			log.Printf("could not remove %s: %v", tmpDir, err)
		}
	}()

	img, err := image.Generate(image.Params{
		Logo:       *logo,
		Text:       fmt.Sprintf("%d: %s", ep.Number, ep.Title),
		Font:       *font,
		Foreground: foreground,
		Background: background,
		Width:      *width,
		Height:     *height,
	})
	if err != nil {
		return fmt.Errorf("could not generate image: %v", err)
	}

	// We create the image and store it in the temp directory.
	slide := filepath.Join(tmpDir, "slide.png")
	if err := writePNG(slide, img); err != nil {
		return fmt.Errorf("could not create image: %v", err)
	}

	// Then we create the video.
	vid := filepath.Join(tmpDir, "vid.mp4")
	if err := ffmpeg(slide, ep.MP3, vid); err != nil {
		return fmt.Errorf("could not create video: %v\n", err)
	}

	// We generate the metadata for the YouTube upload.
	var buf bytes.Buffer
	if err := titleTmpl.Execute(&buf, ep); err != nil {
		return fmt.Errorf("could not create video title from template: %v", err)
	}

	// We drop all the HTML tags and line breaks from the description.
	desc := bluemonday.StrictPolicy().Sanitize(ep.Desc)
	desc = strings.Replace(desc, "\n", " ", -1)
	data := &youtube.Video{
		Snippet: &youtube.VideoSnippet{
			Title:       buf.String(),
			Description: fmt.Sprintf("Original post: %s\n\n", ep.Link) + desc,
			Tags:        append(ep.Tags, strings.Split(*tags, ",")...),
		},
		Status: &youtube.VideoStatus{PrivacyStatus: "unlisted"},
	}

	// And finally we upload the video to YouTube.
	if err := upload(client, data, vid); err != nil {
		return fmt.Errorf("could not upload to YouTube: %v", err)
	}
	return nil
}

// writePNG encodes the given image as a PNG file at the given path.
func writePNG(path string, img stdimage.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("could not create %s: %v", path, err)
	}

	if err := png.Encode(f, img); err != nil {
		return fmt.Errorf("could not encode to %s: %v", path, err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("could not close %s: %v", path, err)
	}
	return nil
}

// ffmpeg creates a video at the filepath vid. The generated video
// has the image at the the img filepath as fixed background and plays the
// audio at the mp3 filepath.
// This function requires ffmpeg to be installed.
// See https://ffmpeg.org for installation instructions.
func ffmpeg(img, mp3, vid string) error {
	// ffmpeg -y -i slide.png -i audio.mp3 -pix_fmt yuv420p -c:a aac -c:v libx264 -crf 18 out.mp4
	cmd := exec.Command("ffmpeg", "-y", "-loop", "1", "-i", img, "-i", mp3, "-shortest",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-crf", "18",
		vid)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// upload uploads the video in the given path to YouTube with the given details.
func upload(client *http.Client, data *youtube.Video, path string) error {
	service, err := youtube.New(client)
	if err != nil {
		return fmt.Errorf("could not create YouTube client: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("could not open %v: %v", path, err)
	}
	defer f.Close()

	call := service.Videos.Insert("snippet,status", data)
	_, err = call.Media(f).Do()
	return err
}
