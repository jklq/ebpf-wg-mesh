package delivery

import platformv1 "ebof-wg-mesh/api/proto/platformv1"

const (
	defaultServiceCPUMillis       int64 = 250
	defaultServiceMemoryMebibytes int64 = 256
)

func directImageServiceSpec(image string, runtime *platformv1.ServiceRuntime) *platformv1.ServiceSpec {
	if runtime == nil {
		runtime = defaultServiceRuntime()
	}
	return &platformv1.ServiceSpec{
		Runtime: runtime,
		Source: &platformv1.ServiceSource{
			Source: &platformv1.ServiceSource_Image{
				Image: &platformv1.DirectImageSource{Image: image},
			},
		},
	}
}

func repositoryServiceSpec(runtime *platformv1.ServiceRuntime, source *platformv1.ServiceSourceSpec) *platformv1.ServiceSpec {
	if runtime == nil {
		runtime = defaultServiceRuntime()
	}
	if source == nil {
		source = &platformv1.ServiceSourceSpec{}
	}
	return &platformv1.ServiceSpec{
		Runtime: runtime,
		Source: &platformv1.ServiceSource{
			Source: &platformv1.ServiceSource_SourceSpec{
				SourceSpec: source,
			},
		},
	}
}

func defaultServiceRuntime() *platformv1.ServiceRuntime {
	return &platformv1.ServiceRuntime{
		CpuMillis:       defaultServiceCPUMillis,
		MemoryMebibytes: defaultServiceMemoryMebibytes,
	}
}
