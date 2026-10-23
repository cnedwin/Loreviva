# Design and Repository Support

Updated21 October2026.

Product design3.0.2 defines the full world-authoring, character, memory and customer-integration goals. The approved machine-contract baseline remains1.4.0 (58 operations,156 schemas, migrations0001–0007); this checkout now also contains the Task8 development snapshot, whose working contract is1.6.0 (67 operations,183 schemas, migrations0001–0008). The two must not be conflated: a working contract in the tree does not mean1.6.0 or complete Task8 is accepted. Reserved contract operations do not imply registered handlers. R0 support is the verified **Windows local fixed-volume** path: the durability and coordinator layer refuses to open a non-Windows volume, so unit packages and the maintenance journal are exercised on Windows only, while the tree must still build on Linux.

The `.wepkg` static preflight is the one Task9 capability that is implemented end to end (contract 1.6.0 operations `preflightContentPackage` and `getContentPackageCapabilities`); package import, rehearsal and local publish are not.

The SDK, reference-client and self-memory governance implementations entered the tree with that snapshot after finite reviews; complete Task8,1.6.0 as a whole, Tasks9–12, sustained character growth and24 long-term experience scenarios are still not delivered as a complete product. Real-model quality, human experience, the complete installation release, payments and deployment remain unaccepted. Known failures and not-run items for this batch are listed in the[progress note](../../progress/2026-10-21.en.md); a documentation correction does not replace them.

- [Current repository requirements and implementation](../../en/02-requirements.md)
- [Actual HTTP and module support](../../en/03-architecture.md)
- [Baseline setup guide](../../en/04-setup-and-integration.md)
- [Product design](01-product-overview.md),[requirements](02-requirements.md),[architecture](03-architecture.md) and[integration design](04-setup-and-integration.md)
- [Development roadmap](05-roadmap.md)
