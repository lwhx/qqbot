package qqbot

import (
	"github.com/sky22333/qqbot/config"
	"github.com/sky22333/qqbot/message"
)

type Config = config.Config

type TargetType = message.TargetType

const (
	TargetC2C     = message.TargetC2C
	TargetGroup   = message.TargetGroup
	TargetChannel = message.TargetChannel
)

type PushRequest = message.PushRequest
type PushResult = message.PushResult
type DeliveryStatus = message.DeliveryStatus
