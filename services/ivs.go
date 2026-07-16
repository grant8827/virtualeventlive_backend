package services

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ivs"
	"github.com/aws/aws-sdk-go-v2/service/ivs/types"
)

type StreamCredentials struct {
	ChannelARN     string
	IngestURL      string
	StreamKey      string
	PlaybackURL    string
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
		Region: region,
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

	out, err := s.client.CreateChannel(ctx, &ivs.CreateChannelInput{
		Name:        aws.String(eventTitle),
		LatencyMode: types.ChannelLatencyModeLowLatency,
		Type:        types.ChannelTypeStandardChannelType,
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

// IsLive reports whether a channel currently has an active broadcast —
// GetStream returns a ChannelNotBroadcasting error (not a Go error worth
// surfacing) when the host simply isn't streaming right now.
func (s *IVSService) IsLive(ctx context.Context, channelARN string) (bool, error) {
	if !s.Enabled || channelARN == "" {
		return false, nil
	}

	_, err := s.client.GetStream(ctx, &ivs.GetStreamInput{ChannelArn: aws.String(channelARN)})
	if err != nil {
		var notBroadcasting *types.ChannelNotBroadcasting
		if errors.As(err, &notBroadcasting) {
			return false, nil
		}
		return false, fmt.Errorf("IVS GetStream: %w", err)
	}
	return true, nil
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
