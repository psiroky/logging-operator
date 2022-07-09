// Copyright © 2022 Banzai Cloud
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package syslogng

var SyslogNGConfigCheckTemplate = `
# include other config files
@include /fluentd/etc/input.conf
@include /fluentd/etc/generated.conf
@include /fluentd/etc/devnull.conf
@include /fluentd/etc/fluentlog.conf
`

var SyslogNGDefaultTemplate = `
# include other config files
@include /fluentd/etc/input.conf
@include /fluentd/app-config/*
@include /fluentd/etc/devnull.conf
@include /fluentd/etc/fluentlog.conf
`
var SyslogNGInputTemplate = `
# Enable RPC endpoint (this allows to trigger config reload without restart)
<system>
  rpc_endpoint 127.0.0.1:24444
  log_level {{ .LogLevel }}
  workers {{ .Workers }}
{{- if .RootDir }}
  root_dir {{ .RootDir }}
{{- end }}
{{- if .IgnoreRepeatedLogInterval }}
  ignore_repeated_log_interval {{ .IgnoreRepeatedLogInterval }}
{{- end }}
{{- if .IgnoreSameLogInterval }}
  ignore_same_log_interval {{ .IgnoreSameLogInterval }}
{{- end }}
</system>

# Prometheus monitoring
{{ if .Monitor.Enabled }}
<source>
    @type prometheus
    port {{ .Monitor.Port }}
    metrics_path {{ .Monitor.Path }}
</source>
<source>
    @type prometheus_monitor
</source>
<source>
    @type prometheus_output_monitor
</source>
{{ end }}
`
var SyslogNGOutputTemplate = `
<match **>
    @type null
    @id main-no-output
</match>
`

var SyslogNGLog = `
<label @FLUENT_LOG>
  <match fluent.*>
    @type %s
    @id main-fluentd-log
  </match>
</label>
`
