package thumbnail_service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/gif"
	"os"

	"github.com/disintegration/imaging"
	"github.com/sirupsen/logrus"
	"github.com/turt2live/matrix-media-repo/storage"
	"github.com/turt2live/matrix-media-repo/types"
	"github.com/turt2live/matrix-media-repo/util"
)

type generatedThumbnail struct {
	ContentType  string
	DiskLocation string
	SizeBytes    int64
	Animated     bool
}

type thumbnailer struct {
	ctx context.Context
	log *logrus.Entry
}

func NewThumbnailer(ctx context.Context, log *logrus.Entry) *thumbnailer {
	return &thumbnailer{ctx, log}
}

func (t *thumbnailer) GenerateThumbnail(media *types.Media, width int, height int, method string, animated bool, forceGeneration bool) (*generatedThumbnail, error) {
	if animated && !util.ArrayContains(animatedTypes, media.ContentType) {
		t.log.Warn("Attempted to animate a media record that isn't an animated type. Assuming animated=false")
		animated = false
	}

	src, err := imaging.Open(media.Location)
	if err != nil {
		return nil, err
	}

	srcWidth := src.Bounds().Max.X
	srcHeight := src.Bounds().Max.Y

	aspectRatio := float32(srcHeight) / float32(srcWidth)
	targetAspectRatio := float32(width) / float32(height)
	if aspectRatio == targetAspectRatio {
		// Highly unlikely, but if the aspect ratios match then just resize
		method = "scale"
		t.log.Info("Aspect ratio is the same, converting method to 'scale'")
	}

	thumb := &generatedThumbnail{
		Animated: animated,
	}

	if srcWidth <= width && srcHeight <= height {
		if forceGeneration {
			t.log.Warn("Image is too small but the force flag is set. Adjusting dimensions to fit image exactly.")
			width = srcWidth
			height = srcHeight
		} else {
			// Image is too small - don't upscale
			thumb.ContentType = media.ContentType
			thumb.DiskLocation = media.Location
			thumb.SizeBytes = media.SizeBytes
			t.log.Warn("Image too small, returning raw image")
			return thumb, nil
		}
	}

	contentType := "image/png"
	imgData := &bytes.Buffer{}
	if animated && util.ArrayContains(animatedTypes, media.ContentType) {
		t.log.Info("Generating animated thumbnail")
		contentType = "image/gif"

		// Animated GIFs are a bit more special because we need to do it frame by frame.
		// This is fairly resource intensive. The calling code is responsible for limiting this case.

		inputFile, err := os.Open(media.Location)
		if err != nil {
			t.log.Error("Error generating animated thumbnail: " + err.Error())
			return nil, err
		}
		defer inputFile.Close()

		g, err := gif.DecodeAll(inputFile)
		if err != nil {
			t.log.Error("Error generating animated thumbnail: " + err.Error())
			return nil, err
		}

		for i := range g.Image {
			frameThumb, err := thumbnailFrame(g.Image[i], method, width, height, imaging.Lanczos)
			if err != nil {
				t.log.Error("Error generating animated thumbnail frame: " + err.Error())
				return nil, err
			}

			t.log.Info(fmt.Sprintf("Width = %d    Height = %d    FW=%d    FH=%d", width, height, frameThumb.Bounds().Max.X, frameThumb.Bounds().Max.Y))
			g.Image[i] = image.NewPaletted(frameThumb.Bounds(), g.Image[i].Palette)
			draw.Draw(g.Image[i], frameThumb.Bounds(), frameThumb, image.Pt(0, 0), draw.Over)
		}

		err = gif.EncodeAll(imgData, g)
		if err != nil {
			t.log.Error("Error generating animated thumbnail: " + err.Error())
			return nil, err
		}
	} else {
		src, err = thumbnailFrame(src, method, width, height, imaging.Lanczos)
		if err != nil {
			t.log.Error("Error generating thumbnail: " + err.Error())
			return nil, err
		}

		// Put the image bytes into a memory buffer
		err = imaging.Encode(imgData, src, imaging.PNG)
		if err != nil {
			t.log.Error("Unexpected error encoding thumbnail: " + err.Error())
			return nil, err
		}
	}

	// Reset the buffer pointer and store the file
	location, err := storage.PersistFile(imgData, t.ctx)
	if err != nil {
		t.log.Error("Unexpected error saving thumbnail: " + err.Error())
		return nil, err
	}

	fileSize, err := util.FileSize(location)
	if err != nil {
		t.log.Error("Unexpected error getting the size of the thumbnail: " + err.Error())
		return nil, err
	}

	thumb.DiskLocation = location
	thumb.ContentType = contentType
	thumb.SizeBytes = fileSize

	return thumb, nil
}

func thumbnailFrame(src image.Image, method string, width int, height int, filter imaging.ResampleFilter) (image.Image, error) {
	if method == "scale" {
		src = imaging.Fit(src, width, height, filter)
	} else if method == "crop" {
		src = imaging.Fill(src, width, height, imaging.Center, filter)
	} else {
		return nil, errors.New("unrecognized method: " + method)
	}

	return src, nil
}