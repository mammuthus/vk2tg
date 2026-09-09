package main

import (
	"context"
	"net/url"
	"strconv"
)

type VKStickerImage struct {
	URL    string `json:"url"`
	Width  int64  `json:"width"`
	Height int64  `json:"height"`
}

type VKSticker struct {
	ID                   int64            `json:"sticker_id"`
	Images               []VKStickerImage `json:"images"`
	ImagesWithBackground []VKStickerImage `json:"images_with_background"`
}

func (sticker *VKSticker) imageURL() string {
	if sticker == nil {
		return ""
	}
	for _, images := range [][]VKStickerImage{sticker.Images, sticker.ImagesWithBackground} {
		var best VKStickerImage
		for _, image := range images {
			if image.URL != "" && image.Width > 0 && image.Height > 0 && (best.URL == "" || float64(image.Width)*float64(image.Height) > float64(best.Width)*float64(best.Height)) {
				best = image
			}
		}
		if best.URL != "" {
			return best.URL
		}
	}
	return ""
}

func messageStickers(message VKMessage) ([]*VKSticker, error) {
	var stickers []*VKSticker
	var visit func([]VKAttachment, int) error
	var wall func(VKWall, int) error
	wall = func(post VKWall, depth int) error {
		if depth > 8 {
			return ErrVKInvalidResponse
		}
		if err := visit(post.Attachments, depth+1); err != nil {
			return err
		}
		for _, original := range post.CopyHistory {
			if err := wall(original, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	visit = func(attachments []VKAttachment, depth int) error {
		if depth > 8 {
			return ErrVKInvalidResponse
		}
		for _, attachment := range attachments {
			if attachment.Type == "sticker" {
				if attachment.Sticker == nil {
					return ErrVKInvalidResponse
				}
				stickers = append(stickers, attachment.Sticker)
			}
			if attachment.Type == "wall" && attachment.Wall != nil {
				if err := wall(*attachment.Wall, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	err := visit(message.Attachments, 0)
	return stickers, err
}

func (client *VKClient) enrichStickers(ctx context.Context, message VKMessage) error {
	stickers, err := messageStickers(message)
	if err != nil {
		return err
	}
	var missing []*VKSticker
	for _, sticker := range stickers {
		if sticker.imageURL() == "" {
			if sticker.ID <= 0 {
				return ErrVKInvalidResponse
			}
			missing = append(missing, sticker)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	var response struct {
		Items []VKMessage `json:"items"`
	}
	if err := client.callVersion(ctx, "messages.getById", url.Values{"message_ids": {strconv.FormatInt(message.ID, 10)}}, &response, "5.131"); err != nil {
		return err
	}
	if len(response.Items) != 1 || response.Items[0].ID != message.ID || response.Items[0].PeerID != message.PeerID || response.Items[0].FromID != message.FromID {
		return ErrVKInvalidResponse
	}
	resolved, err := messageStickers(response.Items[0])
	if err != nil {
		return err
	}
	for _, target := range missing {
		for _, source := range resolved {
			if source.ID == target.ID && source.imageURL() != "" {
				target.Images, target.ImagesWithBackground = source.Images, source.ImagesWithBackground
				break
			}
		}
		if target.imageURL() == "" {
			return ErrVKInvalidResponse
		}
	}
	return nil
}
