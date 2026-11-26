/*

Transcodes all videos in the given directory and all of it's subdirectories
using ffmpeg.

*/

package main

////////////////////////////////////////////////////////////////////////////
// Porgram: FfCvt
// Purpose: ffmpeg convert wrapper tool
// Authors: Tong Sun (c) 2015-2024, All rights reserved
////////////////////////////////////////////////////////////////////////////

//go:generate sh -x ffcvt_cli.sh

////////////////////////////////////////////////////////////////////////////
// Program start

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

////////////////////////////////////////////////////////////////////////////
// Constant and data type/structure definitions

const _encodedExt = "_.mkv"

// video encompass data around a single video
type video struct {
	name      string
	size, sum int64
	pct       int
}

// videoCol is the collection for the given set of videos
type videoCol struct {
	videos  []video
	skipped []video // inputs skipped on entry (e.g. -nc already done)
	sum     int64
}

////////////////////////////////////////////////////////////////////////////
// Global variables definitions

var (
	version = "1.15.0"
	date    = "2024-02-08"

	encodedExt string = _encodedExt
	totalOrg   int64  = 1
	totalNew   int64  = 1
	vidCol     videoCol
	workDirs   []string
	cutOps     string = ""

	transpFrom, transpTo string
)

var pitch = map[string]string{
	"C":  "261.63",
	"Db": "277.18",
	"D":  "293.66",
	"Eb": "311.13",
	"E":  "329.63",
	"F":  "349.23",
	"Gb": "369.99",
	"G":  "392.00",
	"Ab": "415.30",
	"A":  "440.00",
	"Bb": "466.16",
	"B":  "493.88",
}

var pitchCoefficient = map[int]string{
	1:  "1.059463094359",
	2:  "1.122462048308",
	3:  "1.189207115001",
	4:  "1.259921049893",
	5:  "1.334839854168",
	6:  "1.414213562370",
	7:  "1.498307076873",
	8:  "1.587401051964",
	9:  "1.681792830503",
	10: "1.781797436275",
	11: "1.887748625357",
}

func init() {
	pitch["C#"] = pitch["Db"]
	pitch["D#"] = pitch["Eb"]
	pitch["F#"] = pitch["Gb"]
	pitch["G#"] = pitch["Ab"]
	pitch["A#"] = pitch["Bb"]
}

////////////////////////////////////////////////////////////////////////////
// Main

func main() {

	// == Commandline handling
	if Opts.PrintV {
		fmt.Printf("%s version %s built on %s\n\n", progname, version, date)

		ver, err := getVersion(Opts.FFMpeg)
		if err != nil {
			log.Fatalf("%s: Version check error - %s\n", progname, err.Error())
		}
		fmt.Printf("%s\n\n", ver)

		// ffprobe, only use the first word
		ver, err = getVersion((strings.Fields(Opts.FFProbe))[0])
		if err != nil {
			log.Fatalf("%s: Version check error - %s\n", progname, err.Error())
		}
		fmt.Printf("%s\n", ver)

		os.Exit(0)
	}

	//fmt.Printf("%+v\n", Opts)
	if len(Opts.Seg) > 0 {
		// sanity check
		_, err := time.Parse("15:04:05", Opts.Seg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Seg format error: '%s'\n", Opts.Seg)
			os.Exit(1)
		}
	}

	if len(Opts.Cut) > 0 {
		var b, vc strings.Builder
		var ci int
		var cv string
		d0, _ := time.Parse("15:04:05", "00:00:00")
		for ii, val := range Opts.Cut {
			ci = ii
			cv = val
			cRange := regexp.MustCompile(`\s*(.*?)\s*-\s*(\S*)\s*$`).
				FindStringSubmatch(val)
			if len(cRange) != 3 {
				fmt.Fprintf(os.Stderr, "pair - ")
				goto range_error
			}
			//fmt.Println("Cut range:", cRange[1], cRange[2])
			timeBgn, err := time.Parse("15:04:05", cRange[1])
			if err != nil {
				//fmt.Fprintf(os.Stderr, err.Error())
				fmt.Fprintf(os.Stderr, "start - ")
				goto range_error
			}
			endStr := ""
			if len(cRange[2]) > 0 {
				timeEnd, err := time.Parse("15:04:05", cRange[2])
				if err != nil {
					//fmt.Fprintf(os.Stderr, err.Error())
					fmt.Fprintf(os.Stderr, "end - ")
					goto range_error
				}
				endStr = fmt.Sprintf(":end=%d", int(timeEnd.Sub(d0).Seconds()))
			}
			secBgn := int(timeBgn.Sub(d0).Seconds())
			fmt.Fprintf(&b, "[0:v]trim=start=%d%s,setpts=PTS-STARTPTS[v%d];"+
				"[0:a]atrim=start=%d%s,asetpts=PTS-STARTPTS[a%d];",
				secBgn, endStr, ii, secBgn, endStr, ii)
			fmt.Fprintf(&vc, "[v%d][a%d]", ii, ii)
		}

		//fmt.Println("Cut(s):", len(Opts.Cut), Opts.Cut)
		//fmt.Println(vc.String())
		fmt.Fprintf(&b, "%sconcat=n=%d:v=1:a=1[vo][ao]", vc.String(), len(Opts.Cut))
		//fmt.Println(b.String())
		cutOps = b.String()
		goto cut_ok
	range_error:
		fmt.Fprintf(os.Stderr, "Cut range %d format error for '%s'\n", ci, cv)
		os.Exit(1)
	}
cut_ok:

	// One mandatory arguments, either -d or -f
	if len(Opts.Directory)+len(Opts.File) < 1 {
		Usage()
	}
	if len(Opts.TranspFrom) != 0 && len(Opts.TranspTo) == 0 ||
		len(Opts.TranspFrom) == 0 && len(Opts.TranspTo) != 0 {
		fmt.Fprintf(os.Stderr, "Signature transposition needs transpose keys for both from and to\n")
		os.Exit(1)
	}
	if len(Opts.TranspFrom) != 0 {
		var ok bool
		if transpFrom, ok = pitch[Opts.TranspFrom]; !ok {
			fmt.Fprintf(os.Stderr, "Unknown key signature to transpose from\n")
			os.Exit(1)
		}
		if transpTo, ok = pitch[Opts.TranspTo]; !ok {
			fmt.Fprintf(os.Stderr, "Unknown key signature to transpose to\n")
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Transposing from '%s' (%s) to '%s' (%s)\n",
			Opts.TranspFrom, transpFrom, Opts.TranspTo, transpTo)
	}
	if Opts.TranspBy != 0 {
		if Opts.TranspBy > 0 {
			transpFrom, transpTo = "1", pitchCoefficient[Opts.TranspBy]
		} else {
			transpFrom, transpTo = pitchCoefficient[-Opts.TranspBy], "1"
		}
		if transpFrom == "" || transpTo == "" {
			fmt.Fprintf(os.Stderr, "Error: TranspBy value not within +-1~11\n")
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Transposing by %d (%s->%s)\n",
			Opts.TranspBy, transpFrom, transpTo)
	}

	encodedExt = Opts.Ext
	if Opts.Karaoke && encodedExt == _encodedExt {
		encodedExt = "_karaoke.mp4"
	}
	// Sanity check
	if Opts.WDirectory != "" {
		// To error on the safe side -- when -d is not given but -f is,
		// path.Clean(Opts.Directory) will return ".", thus forcing
		// the work directory cannot be the same as pwd
		// because the encodedExt might be conflicting with the source file
		encodedExt = encodedExt[1:] // now ".mkv"
		Opts.Directory = path.Clean(Opts.Directory)
		Opts.WDirectory = path.Clean(Opts.WDirectory)
		absd, _ := filepath.Abs(Opts.Directory)

		// The basename of the source directory will be created under the work
		//directory, which will become the new work directory
		if Opts.File == "" {
			Opts.WDirectory += string(os.PathSeparator) + filepath.Base(absd)
		}
		absw, _ := filepath.Abs(Opts.WDirectory)
		if absd == absw {
			log.Fatalf("[%s] Error: work directory\n\t\t  (%s)\n\t\t is the same as the source directory\n\t\t  (%s).", progname, absw, absd)
		}

		debug("Transcoding to "+Opts.WDirectory, 2)
		err := os.MkdirAll(Opts.WDirectory, os.ModePerm)
		checkError(err)
	} else {
		Opts.Par2C = false
	}

	setupSignals()
	startTime := time.Now()
	exitCode := exitOK
	// transcoding
	if Opts.File != "" {
		fmt.Printf("\n== Transcoding: %s\n", Opts.File)
		transcodeFile(batchCancel.ctx(), Opts.File)
	} else if Opts.Directory != "" {
		// Load the durable state before queueing so -nc can recognize
		// inputs already finished by an earlier run.
		rerunJournal = loadJournal()
		filepath.Walk(Opts.Directory, visit)
		// runBatch computes per-file percentages, tracks outcomes in
		// the journal, and prints the reconciling end-of-run summary.
		exitCode = runBatch(startTime)
	}
	// par2 creating
	if Opts.Par2C {
		filepath.Walk(Opts.WDirectory, visitWDir)
		createPar2s(workDirs)
	}
	// reporting — single-file mode still gets the legacy totals; the
	// directory mode prints its full summary inside runBatch.
	if Opts.File != "" {
		fmt.Printf("\nTranscoding completed in %s\n", time.Since(startTime))
		fmt.Printf("Org Size: %d MB\n", totalOrg/1024)
		fmt.Printf("New Size: %d MB\n", totalNew/1024)
		fmt.Printf("Saved:    %d%%\n",
			(totalOrg-totalNew)*100/totalOrg)
	}
	os.Exit(exitCode)
}

////////////////////////////////////////////////////////////////////////////
// Methods definitions

// Append the video file to the list, unless it's encoded already
// and duplicate none-video files to dest dir as well
func (vidCol *videoCol) append(p string, size int64) {
	// filepath.Walk hands back paths already anchored at the walk root,
	// so join defensively: on Windows, joining an absolute drive path
	// onto the root would duplicate the root rather than replace it.
	fname := p
	if !filepath.IsAbs(p) {
		fname = filepath.Join(Opts.Directory, p)
	}

	if Opts.WDirectory == "" && fname[len(fname)-5:] == encodedExt {
		debug("Already-encoded file ignored: "+fname, 1)
		return
	}

	fext := strings.ToUpper(fname[len(fname)-4:])
	if strings.Index(Opts.Exts, fext) < 0 {
		// None-video files, dup to dest, hardlink 1st else copy
		if Opts.WDirectory != "" {
			src := fname
			dst := filepath.Join(Opts.WDirectory, fname)
			err := linkFile(src, dst)
			if err != nil {
				copyFile(src, dst)
			}
		}
		debug("None-video file '"+fname+"' duplicated to dest dir.", 1)
		return
	}

	if !Opts.Links && isSymlink(fname) {
		debug("Skip symlink file: "+fname, 1)
		return
	}

	if Opts.NoClobber && isAlreadyDone(fname) {
		// A prior run completed this file. Record it as a skipped input
		// so the summary counts and byte totals reconcile on rerun.
		vidCol.skipped = append(vidCol.skipped,
			video{name: fname, size: size})
		debug("Encoded file exist for: "+fname, 1)
		return
	}

	vidCol.sum += size
	vidCol.videos = append(vidCol.videos,
		video{name: fname, size: size, sum: vidCol.sum})

}

////////////////////////////////////////////////////////////////////////////
// Function definitions

//==========================================================================
// Directory & files handling

// visit will visit the source directory and queue videos found there to vidCol
func visit(path string, f os.FileInfo, err error) error {
	if f.IsDir() {
		return nil
	}

	vidCol.append(path, f.Size())
	return nil
}

// visitWDir will visit the work dir and add all directories there to workDirs
func visitWDir(path string, f os.FileInfo, err error) error {
	if !f.IsDir() {
		return nil
	}

	debug(path, 2)
	workDirs = append(workDirs, path)
	return nil
}

// createPar2s will create par2s files for each dir in workDirs
func createPar2s(workDirs []string) {
	fmt.Printf("\n== Creating par2 files\n\n")
	for ii, dir := range workDirs {
		if ii == 0 && len(workDirs) > 1 {
			// skip the root folder, if there are sub folders
			continue
		}
		os.Chdir(dir)
		dirName := filepath.Base(dir)

		// create par2s for all files within dest dir
		cmd := []string{"par2create", "-u", "zz_" + dirName + ".par2", "*"}
		debug(strings.Join(cmd, " "), 1)

		out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
		if err != nil {
			log.Printf("%s: Exec error - %s", progname, err.Error())
		}
		fmt.Printf("%s\n", out)
	}
}

//==========================================================================
// Transcode handling

func transcodeFile(ctx context.Context, inputName string) (
	FileResult, int64, string) {
	//debug(fmt.Sprintf("Transcoding file, NoClobber = %v\n", Opts.NoClobber), 4)
	startTime := time.Now()
	outputName, outputGrpName := getOutputName(inputName), ""
	if Opts.NoClobber && fileExist(outputName) {
		debug("Aborted as the encoded output file exist: "+inputName, 1)
		return ResultSkip, 0, ""
	}
	if len(Opts.Seg) > 0 {
		outputName, outputGrpName = getOutputNameSeg(inputName)
	}
	debug(outputName, 4)
	os.MkdirAll(filepath.Dir(outputName), os.ModePerm)
	var oldAEP, oldVEP, oldSEP string
	oldEPUsed := false

	if !Opts.NoExec {
		// probe the file stream info first, only when not using -n
		fsinfo, err := probeFile(inputName)
		if err != nil {
			msg := fmt.Sprintf("Probe error - %s", err.Error())
			log.Printf("%s: %s", progname, msg)
			return ResultFailure, 0, msg
		}
		debug(fsinfo, 4)
		/*

		   Cases when `-map`s are necessary to deal with here

		   - more than one audio stream, and we pick eng stream only
		   - more than one subtitle stream, and
		     * output all subtitle streams (default, no -sel), or
		     * pick specific subtitle stream(s) via -sel

		*/
		allAudioStreams := regexp.MustCompile(`Stream #0:.+: Audio: (.+)`).
			FindAllStringSubmatch(fsinfo, -1)
		if len(allAudioStreams) > 1 ||
			len(regexp.MustCompile(`Stream #0:.+: Subtitle: (.+)`).
				FindAllStringSubmatch(fsinfo, -1)) > 1 {
			// then use the designated audio stream language
			// via *temporarily* using the AEP/VEP/SEP setting
			oldAEP, oldVEP, oldSEP = Opts.AEP, Opts.VEP, Opts.SEP
			oldEPUsed = true
			Opts.VEP += " -map 0:v"
			Opts.AEP += " -map 0:a:m:language:" + Opts.Lang
			//log.Printf("%s: Opts.SEL - %#v", progname, Opts.SEL)
			if len(Opts.SEL) == 0 {
				Opts.SEP += " -map 0:s"
			} else {
				for _, val := range Opts.SEL {
					Opts.SEP += " -map 0:s:m:language:" + val
				}
			}
		} else if len(allAudioStreams) > 0 {
			debug(inputName+" has single audio stream", 2)
			dealSurroundSound(allAudioStreams[0][1])
		}
	}

	args := []string{"-hide_banner", "-i", inputName}
	args = append(args, strings.Fields(Opts.OptExtra)...)
	//debug(strings.Join(args, " "), 4)
	args = encodeParametersS(encodeParametersA(encodeParametersV(args)))
	if Opts.Karaoke {
		args = append(args, "-filter_complex")
		args = append(args, "[0:1]pan=mono|c0=c0-c1[a]")
		args = append(args, "-map")
		args = append(args, "0:0")
		args = append(args, "-map")
		args = append(args, "[a]")
		args = append(args, "-map")
		args = append(args, "0:1")
	}
	if len(transpFrom) != 0 {
		args = append(args, "-af")
		af := fmt.Sprintf("rubberband=pitch=%[2]s/%[1]s",
			transpFrom, transpTo)
		args = append(args, af)
	}
	if Opts.Force {
		args = append(args, "-y")
	}
	if len(Opts.Seg) > 0 {
		args = append(args, "-f")
		args = append(args, "segment")
		args = append(args, "-segment_time")
		args = append(args, Opts.Seg)
		args = append(args, "-reset_timestamps")
		args = append(args, "1")
	}
	if len(Opts.Speed) > 0 {
		args = append(args, "-filter_complex")
		args = append(args, fmt.Sprintf("[0:v]setpts=PTS/%s[v];[0:a]atempo=%s[a]",
			Opts.Speed, Opts.Speed))
		args = append(args, "-map")
		args = append(args, "[v]")
		args = append(args, "-map")
		args = append(args, "[a]")
	}
	if len(cutOps) != 0 {
		args = append(args, "-filter_complex")
		args = append(args, cutOps)
		args = append(args, "-map")
		args = append(args, "[vo]")
		args = append(args, "-map")
		args = append(args, "[ao]")
	}
	args = append(args, flag.Args()...)

	if Opts.NoExec {
		// Dry run: print the command that would run, touch nothing.
		dryTarget := outputName
		if len(Opts.Seg) > 0 {
			dryTarget = outputGrpName
		}
		args = append(args, dryTarget)
		fmt.Printf("%s: to execute -\n  %s %s\n",
			progname, Opts.FFMpeg, strings.Join(args, " "))
		return ResultSkip, 0, ""
	}

	// Real run: transcode into unique temp path(s) in the destination
	// directory, so a crash or failure can never leave a partial file
	// under the final output name. Segment mode uses a temp printf
	// pattern; normal mode uses a single temp file.
	finalName := outputName
	var tempName, tempGrpName string
	if len(Opts.Seg) > 0 {
		tempGrpName = tempSegmentGroupName(outputGrpName)
		tempName = strings.Replace(tempGrpName, "%02d", "00", 1)
	} else {
		tempName = tempOutputName(outputName)
	}
	args = append(args, tempName)
	if len(Opts.Seg) > 0 {
		// The segment muxer needs the printf pattern as its last arg.
		args[len(args)-1] = tempGrpName
	}
	debug(Opts.FFMpeg+" "+strings.Join(args, " "), 1)

	// Cleanup helper for failure paths: remove every partial artifact,
	// including segment files when segmenting.
	cleanup := func() {
		if len(Opts.Seg) > 0 {
			removeSegments(tempGrpName)
			return
		}
		_ = os.Remove(tempName)
	}

	cmd := exec.CommandContext(ctx, Opts.FFMpeg, args...)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	runErr := cmd.Run()

	timeTake := time.Since(startTime)
	if runErr != nil {
		log.Printf("%s: Exec error - %s\n\n%s", progname,
			runErr.Error(), string(errOut.Bytes()))
		fmt.Printf("%s\n", out.String())
		if ctx.Err() != nil {
			fmt.Println("Interrupted.")
			cleanup()
			return ResultFailure, 0, "interrupted: transcode aborted"
		}
		fmt.Println("Failed.")
		// Unconditionally remove the partial output. The old code only
		// removed files <=500 bytes, which left large corrupt half-files
		// behind for -nc to mistake for finished work.
		cleanup()
		debug("Failed output file '"+tempName+"' removed.", 1)
		return ResultFailure, 0, runErr.Error()
	}

	// Verify the output is complete and readable: non-empty and
	// readable end to end. Segment mode verifies every produced part
	// and then atomically moves each into its final name.
	if len(Opts.Seg) > 0 {
		parts, err := segmentFiles(tempGrpName)
		if err != nil || len(parts) == 0 {
			cleanup()
			return ResultFailure, 0, "no segment output produced"
		}
		var total int64
		for _, part := range parts {
			size, verr := verifyReadable(part)
			if verr != nil {
				cleanup()
				return ResultFailure, 0, "unreadable segment output: " + verr.Error()
			}
			total += size
		}
		// Move each verified temp part onto its final counterpart. The
		// final names are derived by index from the printf pattern (they
		// do not exist on disk yet, so they cannot be discovered by glob).
		tempParts, err := segmentFiles(tempGrpName)
		if err != nil {
			cleanup()
			return ResultFailure, 0, "segment glob error: " + err.Error()
		}
		for i, part := range tempParts {
			finalPart := segmentPartNameFromIndex(outputGrpName, i)
			if rerr := atomicRename(part, finalPart); rerr != nil {
				cleanup()
				return ResultFailure, 0, "segment rename error: " + rerr.Error()
			}
		}
		fmt.Printf("%s\n", out.String())
		fmt.Println("Done.")
		originalSize := fileSizeBytes(inputName)
		printFileSizes(originalSize, total, timeTake)
		totalOrg += originalSize
		totalNew += total
		return ResultSuccess, total, ""
	}

	outSize, err := verifyReadable(tempName)
	if err != nil {
		cleanup()
		return ResultFailure, 0, "output not readable: " + err.Error()
	}
	if outSize == 0 {
		cleanup()
		return ResultFailure, 0, "empty output file"
	}

	// Atomically place the verified output under its final name.
	if err := atomicRename(tempName, finalName); err != nil {
		cleanup()
		return ResultFailure, 0, "rename output error: " + err.Error()
	}

	fmt.Printf("%s\n", out.String())
	originalSize := fileSizeBytes(inputName)
	sizeDifference := originalSize - outSize
	totalOrg += originalSize
	totalNew += outSize

	fmt.Println("Done.")
	printFileSizes(originalSize, outSize, timeTake)
	fmt.Printf("Saved:    %d%% with %d KB\n",
		sizeDifference*100/safeDivisor(originalSize), sizeDifference)
	fmt.Printf("Time: %v at %v\n\n", timeTake,
		time.Now().Format("2006-01-02 15:04:05"))

	if oldEPUsed {
		// restored *temporarily* tweaked AEP/VEP/SEP setting
		Opts.AEP, Opts.VEP, Opts.SEP = oldAEP, oldVEP, oldSEP
	}
	return ResultSuccess, outSize, ""
}

func safeDivisor(n int64) int64 {
	if n == 0 {
		return 1
	}
	return n
}

func printFileSizes(originalSize, newSize int64, timeTake time.Duration) {
	fmt.Printf("Org Size: %d KB\n", originalSize/1024)
	fmt.Printf("New Size: %d KB\n", newSize/1024)
}

// tempOutputName gives a unique, same-directory temp path so that the
// final rename is an atomic same-filesystem operation.
func tempOutputName(outputName string) string {
	return fmt.Sprintf("%s.ffcvt-tmp-%d-%d%s",
		strings.TrimSuffix(outputName, filepath.Ext(outputName)),
		os.Getpid(), time.Now().UnixNano(), filepath.Ext(outputName))
}

// tempSegmentGroupName turns a final segment printf pattern
// (dir/name%02d_.mkv) into a unique temp printf pattern
// (dir/name.ffcvt-tmp-<pid>-<n>%02d_.mkv) in the same directory.
func tempSegmentGroupName(groupName string) string {
	dir, file := filepath.Split(groupName)
	idx := strings.Index(file, "%0")
	if idx < 0 {
		return tempOutputName(groupName)
	}
	tempFile := fmt.Sprintf("%s.ffcvt-tmp-%d-%d%s",
		file[:idx], os.Getpid(), time.Now().UnixNano(), file[idx:])
	return filepath.Join(dir, tempFile)
}

// atomicRename renames src to dst, replacing an existing dst. On
// Windows os.Rename fails if dst exists, so remove it first; the output
// lock guarantees no other process is writing the same destination.
func atomicRename(src, dst string) error {
	if err := os.Rename(src, dst); err != nil {
		if rmErr := os.Remove(dst); rmErr != nil && !os.IsNotExist(rmErr) {
			return err
		}
		return os.Rename(src, dst)
	}
	return nil
}

// verifyReadable opens the file, stats it and reads it to the end,
// returning its size. A truncated or unreadable file fails verification.
func verifyReadable(fname string) (int64, error) {
	f, err := os.Open(fname)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if _, err := io.Copy(io.Discard, f); err != nil {
		return 0, err
	}
	return stat.Size(), nil
}

// segmentFiles lists the segment parts matching the ffmpeg segment
// group pattern (e.g. dir/name%02d_.mkv -> dir/name*_.mkv).
func segmentFiles(groupName string) ([]string, error) {
	dir, file := filepath.Split(groupName)
	pattern := stripPrintfVerb(file)
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		return nil, err
	}
	return matches, nil
}

// stripPrintfVerb replaces an ffmpeg-style printf verb such as %02d
// with a single glob wildcard, leaving the rest of the name intact.
func stripPrintfVerb(name string) string {
	idx := strings.Index(name, "%")
	if idx < 0 {
		return name
	}
	end := idx + 1
	for end < len(name) && name[end] != 'd' {
		end++
	}
	if end < len(name) && name[end] == 'd' {
		end++
	}
	return name[:idx] + "*" + name[end:]
}

// segmentPartNameFromIndex fills the printf verb in a segment pattern
// with the given index, e.g. dir/name%02d_.mkv at 0 -> dir/name00_.mkv.
func segmentPartNameFromIndex(pattern string, idx int) string {
	pos := strings.Index(pattern, "%")
	if pos < 0 {
		return pattern
	}
	end := pos + 1
	width := 0
	for end < len(pattern) && pattern[end] >= '0' && pattern[end] <= '9' {
		width = width*10 + int(pattern[end]-'0')
		end++
	}
	if end < len(pattern) && pattern[end] == 'd' {
		end++
	}
	if width == 0 {
		width = 2
	}
	return pattern[:pos] + fmt.Sprintf("%0*d", width, idx) + pattern[end:]
}

func removeSegments(groupName string) {
	parts, err := segmentFiles(groupName)
	if err != nil {
		return
	}
	for _, p := range parts {
		_ = os.Remove(p)
	}
}

// dealSurroundSound will append to Opts.AEP proper setting to encode
// 5.1 surround sound channels
func dealSurroundSound(channelFeatures string) {
	if regexp.MustCompile(`, 5.1\(side\), `).MatchString(channelFeatures) {
		Opts.AEP += " -ac 2"
	}
}

func probeFile(inputName string) (string, error) {
	// Invoke ffprobe directly (no intermediate shell) so behavior is
	// identical on Windows and Unix and fake ffprobe tools can stand in
	// for the real one during testing.
	ffArgs := strings.Fields(Opts.FFProbe)
	ffArgs = append(ffArgs, inputName)
	cmd := exec.Command(ffArgs[0], ffArgs[1:]...)
	out, err := cmd.CombinedOutput()
	// Keep only the "Stream #" lines, matching the historical grep.
	var kept []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "Stream #") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n"), err
}

func getVersion(execName string) (string, error) {
	out := &bytes.Buffer{}

	cmd := exec.Command(execName, "-version")
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	// OK, then only return the first two lines
	lines := strings.Split(string(out.Bytes()), "\n")
	return strings.Join(lines[:2], "\n"), nil
}

// Returns the encode parameters for Subtitle
func encodeParametersS(args []string) []string {
	if Opts.SEP != "" {
		args = append(args, strings.Fields(Opts.SEP)...)
	}
	if Opts.SES != "" {
		args = append(args, strings.Fields(Opts.SES)...)
	}
	return args
}

// Returns the encode parameters for Audio
func encodeParametersA(args []string) []string {
	if Opts.AEP != "" {
		args = append(args, strings.Fields(Opts.AEP)...)
	}
	if Opts.AC {
		args = append(args, "-c:a", "copy")
		return args
	}
	if Opts.AN {
		args = append(args, "-an")
		return args
	}
	if Opts.A2Opus {
		Opts.AES = "libopus"
	}
	//fmt.Printf("] %v + AE: '%s' '%s'\n", args, Opts.AES, Opts.ABR)
	if Opts.AES != "" {
		args = append(args, "-c:a", Opts.AES)
	}
	if Opts.ABR != "" {
		args = append(args, "-b:a", Opts.ABR)
	}
	if Opts.AEA != "" {
		args = append(args, strings.Fields(Opts.AEA)...)
	}
	return args
}

// Returns the encode parameters for Video
func encodeParametersV(args []string) []string {
	//debug(Opts.VES, 4)
	if Opts.VEP != "" {
		args = append(args, strings.Fields(Opts.VEP)...)
	}
	if Opts.VC {
		args = append(args, "-c:v", "copy")
		return args
	}
	if Opts.VN {
		args = append(args, "-vn")
		return args
	}
	if Opts.V2X265 {
		Opts.VES = "libx265"
	}
	if Opts.VES != "" {
		args = append(args, "-c:v", Opts.VES)
	}
	libx2x := regexp.MustCompile(`^libx26(.)`).FindStringSubmatch(Opts.VES)
	if len(libx2x) >= 2 { // VES is either libx264 or libx265
		// check for their specific CRF env var setting, FFCVT_CRF[45]
		libx2t := libx2x[1]
		if len(os.Getenv("FFCVT_CRF"+libx2t)) != 0 {
			Opts.CRF = os.Getenv("FFCVT_CRF" + libx2t)
			debug(Opts.CRF, 3)
		}
		args = append(args, "-"+Opts.VES[3:]+"-params", "crf="+Opts.CRF)
	}
	if Opts.CRF != "" {
		if Opts.VES == "libvpx-vp9" {
			// -b:v 0 -crf 37
			args = append(args, "-b:v", "0", "-crf", Opts.CRF)
		}
	}
	if Opts.VEA != "" {
		args = append(args, strings.Fields(Opts.VEA)...)
	}
	return args
}

//==========================================================================
// Dealing with Files

// Returns true if the file is symbolic link
func isSymlink(fname string) bool {
	fi, err := os.Lstat(fname)
	checkError(err)
	return fi.Mode()&os.ModeSymlink != 0
}

// Returns true if the file exist
func fileExist(fname string) bool {
	_, err := os.Stat(fname)
	return err == nil
}

// Returns the file size in bytes
func fileSizeBytes(fname string) int64 {
	stat, err := os.Stat(fname)
	checkError(err)

	return stat.Size()
}

// Replaces the file extension from the input string with _.mkv, and optionally
// Opts.Suffix as well. If "-w" is defined, use it for output name.
func getOutputName(input string) string {
	index := strings.LastIndex(input, ".")
	if index > 0 {
		input = input[:index]
	}
	r := input + Opts.Suffix + encodedExt
	//fmt.Printf("] (r, od, owd) %+v, %+v, %+v\n", r, Opts.Directory, Opts.WDirectory)
	if Opts.WDirectory != "" {
		// transcoding single file
		if Opts.File != "" {
			r = filepath.Join(Opts.WDirectory,
				filepath.Base(input)+Opts.Suffix+encodedExt)
		} else {
			// Re-root the output under the work directory while
			// preserving the relative sub-path of the input. Using a
			// string replace breaks on absolute paths (Windows drives).
			rel, err := filepath.Rel(Opts.Directory, input)
			if err != nil {
				r = strings.Replace(r, Opts.Directory, Opts.WDirectory, 1)
			} else {
				r = filepath.Join(Opts.WDirectory, rel+Opts.Suffix+encodedExt)
			}
		}
	}
	return r
}

// getOutputNameSeg will do getOutputName() but tailored toward video segmenting.
// The first return will be the first video segment file name, 00_.mkv, while
// the second return will be the segment group file name, %02d_.mkv
func getOutputNameSeg(input string) (string, string) {
	r := getOutputName(input)
	fileFirst := strings.Replace(r, encodedExt, "00"+encodedExt, 1)
	fileGroup := strings.Replace(r, encodedExt, "%02d"+encodedExt, 1)
	return fileFirst, fileGroup
}

//==========================================================================
// Support functions

func debug(input string, threshold int) {
	if !(Opts.Debug >= threshold) {
		return
	}
	print("] ")
	print(input)
	print("\n")
}

func checkError(err error) {
	if err != nil {
		log.Printf("%s: Fatal error - %s", progname, err.Error())
		os.Exit(1)
	}
}
