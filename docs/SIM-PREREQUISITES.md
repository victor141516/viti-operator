# SIM prerequisites

The carrier must support the required Wi-Fi Calling/IMS flows. Android remote
authentication requires a compatible device and the documented Shizuku setup.
For the USB adapter, first install the dedicated
[OpenStick Stock Android Bridge Binaries v1.0.0 component release](https://github.com/victor141516/viti-operator/releases/tag/openstick-android-bridge-v1.0.0).
Download and extract its ZIP, then run `python install.py --check` followed by
`python install.py` with Python 3.10+ and ADB installed. See the
[bridge setup and compatibility guide](../tools/openstick-android-bridge/README.md).
The historical `openstick` adapter name refers here to HiMI/UFI hardware running
the verified **stock Android 4.4.4 firmware**, not the Linux OpenStick OS.

The release contains the recovered privileged APK and two native helper binaries,
plus their installer. Their original source is currently unavailable; recovering
or reimplementing it remains pending. No firmware or private device data is shipped.
The installer rejects unvalidated firmware/ordinary phones and does not flash or
root devices. Remote Android, command adapters and optional PC/SC remain alternative
SIM authentication paths.
