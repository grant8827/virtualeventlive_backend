package services

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ivs"
	"github.com/aws/aws-sdk-go-v2/service/ivs/types"
)

var invalidIVSChannelNameChars = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

type StreamCredentials struct {
	ChannelARN  string
	IngestURL   string
	StreamKey   string
	PlaybackURL string
}

type IVSService struct {
	client  *ivs.Client
	Enabled bool
}

func NewIVSService(accessKeyID, secretKey, region string) *IVSService {
	if accessKeyID == "" || secretKey == "" {
		return &IVSService{Enabled: false}
	}
	cfg := aws.Config{
		Region:      region,
		Credentials: credentials.NewStaticCredentialsProvider(accessKeyID, secretKey, ""),
	}
	return &IVSService{
		client:  ivs.NewFromConfig(cfg),
		Enabled: true,
	}
}

func (s *IVSService) ProvisionChannel(ctx context.Context, eventTitle string) (*StreamCredentials, error) {
	if !s.Enabled {
		return nil, fmt.Errorf("IVS not configured — set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY")
	}

	// LOW latency and STANDARD type are the IVS defaults. Supplying only the
	// sanitized name avoids account/SDK validation differences for optional
	// enum fields while retaining the desired channel configuration.
	out, err := s.client.CreateChannel(ctx, &ivs.CreateChannelInput{
		Name: aws.String(ivsChannelName(eventTitle)),
	})
	if err != nil {
		return nil, fmt.Errorf("IVS CreateChannel: %w", err)
	}

	return &StreamCredentials{
		ChannelARN:  aws.ToString(out.Channel.Arn),
		IngestURL:   aws.ToString(out.Channel.IngestEndpoint),
		StreamKey:   aws.ToString(out.StreamKey.Value),
		PlaybackURL: aws.ToString(out.Channel.PlaybackUrl),
	}, nil
}

func ivsChannelName(eventTitle string) string {
	name := invalidIVSChannelNameChars.ReplaceAllString(strings.TrimSpace(eventTitle), "-")
	name = strings.Trim(name, "-_")
	if name == "" {
		name = "event"
	}
	if len(name) > 128 {
		name = strings.TrimRight(name[:128], "-_")
	}
	return name
}

// IsLive reports whether a channel currently has an active broadcast —
// GetStream returns a ChannelNotBroadcasting error (not a Go error worth
// surfacing) when the host simply isn't streaming right now.
func (s *IVSService) IsLive(ctx context.Context, channelARN string) (bool, error) {
	live, _, err := s.StreamState(ctx, channelARN)
	return live, err
}

// StreamState reports whether the channel is broadcasting and, if so, how
// many people are watching. IVS's viewer count is approximate: a new viewer
// shows up within about 15 seconds of starting playback and is dropped
// within about a minute of stopping.
func (s *IVSService) StreamState(ctx context.Context, channelARN string) (live bool, viewers int64, err error) {
	if !s.Enabled || channelARN == "" {
		return false, 0, nil
	}

	out, err := s.client.GetStream(ctx, &ivs.GetStreamInput{ChannelArn: aws.String(channelARN)})
	if err != nil {
		var notBroadcasting *types.ChannelNotBroadcasting
		if errors.As(err, &notBroadcasting) {
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("IVS GetStream: %w", err)
	}
	if out.Stream != nil {
		viewers = out.Stream.ViewerCount
	}
	return true, viewers, nil
}

// LiveStream is one broadcast that IVS reports as currently on air.
type LiveStream struct {
	ViewerCount int64
	StartedAt   *time.Time
	Health      string
}

// ListLiveStreams returns every stream live on the account right now, keyed
// by channel ARN, so the platform dashboard can show what's on air without
// calling GetStream once per event.
func (s *IVSService) ListLiveStreams(ctx context.Context) (map[string]LiveStream, error) {
	live := map[string]LiveStream{}
	if !s.Enabled {
		return live, nil
	}
	var next *string
	for {
		out, err := s.client.ListStreams(ctx, &ivs.ListStreamsInput{NextToken: next, MaxResults: aws.Int32(100)})
		if err != nil {
			return nil, fmt.Errorf("IVS ListStreams: %w", err)
		}
		for _, st := range out.Streams {
			if st.ChannelArn == nil {
				continue
			}
			live[*st.ChannelArn] = LiveStream{
				ViewerCount: st.ViewerCount,
				StartedAt:   st.StartTime,
				Health:      string(st.Health),
			}
		}
		if out.NextToken == nil || *out.NextToken == "" {
			return live, nil
		}
		next = out.NextToken
	}
}

func (s *IVSService) DeleteChannel(ctx context.Context, channelARN string) error {
	if !s.Enabled {
		return nil
	}
	_, err := s.client.DeleteChannel(ctx, &ivs.DeleteChannelInput{
		Arn: aws.String(channelARN),
	})
	return err
}
