# Third-party source provenance

The bundled engine is adapted from https://github.com/pagecat/vowifi_gateway.
Its MIT license and copyright notice are preserved in `engine/LICENSE`.
Local changes include HTTP SIM authentication, call lifecycle handling,
conferencing, recordings and opt-in diagnostics.

The engine also incorporates SWu-IKEv2-derived code and builds sysmocom Asterisk,
pjproject, PC/SC and other dependencies. Their source references are retained in
the engine Dockerfile and source. These components retain their respective
upstream licenses; the engine MIT notice does not relicense dependencies.

The web voice bundle is built from JsSIP and its dependencies, whose versions
are recorded in `web-src/package-lock.json`. The Gradle wrapper and Android
dependencies retain their upstream licenses and notices.

No new blanket license is assigned to original application code in this
snapshot. Review the complete licensing and attribution position before a
future public distribution.

## Recovered stock-Android bridge release assets

The separate `openstick-android-bridge-v1.0.0` component release contains the
project's recovered laboratory APK and two native helper binaries. Original
source/build inputs are currently unavailable. They are not upstream OpenStick
OS artifacts and are not included as Git source files. The manifest identifies
their exact hashes and APK signing certificate. No private signing key or vendor
firmware/library is distributed. The APK requests privileged telephony access
and includes legacy lab functionality; review its security and licensing before
public redistribution. Recovering or independently reimplementing the source is
pending, not claimed complete by publication of the binaries.
