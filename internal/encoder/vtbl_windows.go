// Code generated from mingw-w64 IDL files by genvtbl.py. DO NOT EDIT.

//go:build windows && (amd64 || arm64)

package encoder

// COM vtable slot indices (IUnknown methods included).
const (
	vIDXGIFactory1_EnumAdapters1                           = 12
	vIDXGIAdapter1_EnumOutputs                             = 7
	vIDXGIOutput1_GetDesc                                  = 7
	vIDXGIOutput1_DuplicateOutput                          = 22
	vIDXGIOutputDuplication_GetDesc                        = 7
	vIDXGIOutputDuplication_AcquireNextFrame               = 8
	vIDXGIOutputDuplication_ReleaseFrame                   = 14
	vID3D11Device_CreateTexture2D                          = 5
	vID3D11DeviceContext_CopyResource                      = 47
	vID3D11VideoDevice_CreateVideoProcessorEnumerator      = 10
	vID3D11VideoDevice_CreateVideoProcessor                = 4
	vID3D11VideoDevice_CreateVideoProcessorInputView       = 8
	vID3D11VideoDevice_CreateVideoProcessorOutputView      = 9
	vID3D11VideoContext_VideoProcessorSetOutputColorSpace  = 15
	vID3D11VideoContext_VideoProcessorSetStreamFrameFormat = 27
	vID3D11VideoContext_VideoProcessorSetStreamColorSpace  = 28
	vID3D11VideoContext_VideoProcessorBlt                  = 53
	vID3D10Multithread_SetMultithreadProtected             = 5
	vIMFAttributes_GetUINT32                               = 7
	vIMFAttributes_GetBlobSize                             = 14
	vIMFAttributes_GetBlob                                 = 15
	vIMFAttributes_SetUINT32                               = 21
	vIMFAttributes_SetUINT64                               = 22
	vIMFAttributes_SetGUID                                 = 24
	vIMFActivate_ActivateObject                            = 33
	vIMFActivate_ShutdownObject                            = 34
	vIMFTransform_GetOutputStreamInfo                      = 7
	vIMFTransform_GetAttributes                            = 8
	vIMFTransform_GetInputAvailableType                    = 13
	vIMFTransform_GetOutputAvailableType                   = 14
	vIMFTransform_SetInputType                             = 15
	vIMFTransform_SetOutputType                            = 16
	vIMFTransform_GetOutputCurrentType                     = 18
	vIMFTransform_ProcessMessage                           = 23
	vIMFTransform_ProcessInput                             = 24
	vIMFTransform_ProcessOutput                            = 25
	vIMFMediaEventGenerator_GetEvent                       = 3
	vIMFMediaEvent_GetType                                 = 33
	vIMFSample_SetSampleTime                               = 36
	vIMFSample_SetSampleDuration                           = 38
	vIMFSample_ConvertToContiguousBuffer                   = 41
	vIMFSample_AddBuffer                                   = 42
	vIMFMediaBuffer_Lock                                   = 3
	vIMFMediaBuffer_Unlock                                 = 4
	vIMFMediaBuffer_SetCurrentLength                       = 6
	vIMF2DBuffer_GetContiguousLength                       = 7
	vIMFDXGIDeviceManager_ResetDevice                      = 7
	vICodecAPI_SetValue                                    = 9
)

// Interface IDs.
var (
	IID_IDXGIFactory1          = mustGUID("{770aae78-f26f-4dba-a829-253c83d1b387}")
	IID_IDXGIAdapter1          = mustGUID("{29038f61-3839-4626-91fd-086879011a05}")
	IID_IDXGIOutput1           = mustGUID("{00cddea8-939b-4b83-a340-a685226666cc}")
	IID_IDXGIOutputDuplication = mustGUID("{191cfac3-a341-470d-b26e-a864f428319c}")
	IID_ID3D11Device           = mustGUID("{db6f6ddb-ac77-4e88-8253-819df9bbf140}")
	IID_ID3D11DeviceContext    = mustGUID("{c0bfa96c-e089-44fb-8eaf-26f8796190da}")
	IID_ID3D11VideoDevice      = mustGUID("{10ec4d5b-975a-4689-b9e4-d0aac30fe333}")
	IID_ID3D11VideoContext     = mustGUID("{61f21c45-3c0e-4a74-9cea-67100d9ad5e4}")
	IID_ID3D10Multithread      = mustGUID("{9b7e4e00-342c-4106-a19f-4f2704f689f0}")
	IID_IMFAttributes          = mustGUID("{2cd2d921-c447-44a7-a13c-4adabfc247e3}")
	IID_IMFActivate            = mustGUID("{7fee9e9a-4a89-47a6-899c-B6A53A70FB67}")
	IID_IMFTransform           = mustGUID("{bf94c121-5b05-4e6f-8000-ba598961414d}")
	IID_IMFMediaEventGenerator = mustGUID("{2cd0bd52-BCD5-4b89-B62C-EADC0C031E7D}")
	IID_IMFMediaEvent          = mustGUID("{DF598932-F10C-4e39-BBA2-C308F101DAA3}")
	IID_IMFSample              = mustGUID("{c40a00f2-b93a-4d80-ae8c-5a1c634f58e4}")
	IID_IMFMediaBuffer         = mustGUID("{045fa593-8799-42b8-BC8D-8968c6453507}")
	IID_IMF2DBuffer            = mustGUID("{7dc9d5f9-9ed9-44ec-9bbf-0600bb589fbb}")
	IID_IMFDXGIDeviceManager   = mustGUID("{eb533d5d-2db6-40f8-97a9-494692014f07}")
	IID_ICodecAPI              = mustGUID("{901db4c7-31ce-41a2-85dc-8fa0bf41b8da}")
	IID_ID3D11Texture2D        = mustGUID("{6f15aaf2-d208-4e89-9ab4-489535d34f9c}")
)
