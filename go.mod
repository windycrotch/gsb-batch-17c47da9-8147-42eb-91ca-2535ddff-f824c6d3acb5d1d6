module zikichombo.org/sio

go 1.27.4

require (
	zikichombo.org/codec v0.0.1-alpha.3
	zikichombo.org/sound v0.1.1-alpha.2
)

replace zikichombo.org/sound => ../temp/deps/sound

replace zikichombo.org/codec => ../temp/deps/codec
