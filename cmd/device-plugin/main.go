/*
 * Copyright © 2021 peizhaoyou <peizhaoyou@4paradigm.com>
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
package main

import (
    "fmt"
    "log"
    "net"
    "os"
    "syscall"

    "4pd.io/k8s-vgpu/pkg/api"
    "4pd.io/k8s-vgpu/pkg/device-plugin"
    "4pd.io/k8s-vgpu/pkg/device-plugin/config"
    "4pd.io/k8s-vgpu/pkg/util"
    "github.com/NVIDIA/go-gpuallocator/gpuallocator"
    "github.com/NVIDIA/gpu-monitoring-tools/bindings/go/nvml"
    "github.com/fsnotify/fsnotify"
    "github.com/spf13/cobra"
    "github.com/spf13/viper"
    "google.golang.org/grpc"
    "k8s.io/klog/v2"
    pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

var (
    failOnInitErrorFlag bool
    //nvidiaDriverRootFlag string
    //enableLegacyPreferredFlag bool
    runtimeSocketFlag string

    rootCmd = &cobra.Command{
        Use:   "scheduler",
        Short: "kubernetes vgpu scheduler",
        Run: func(cmd *cobra.Command, args []string) {
            if err := start(); err != nil {
                klog.Fatal(err)
            }
        },
    }
)

func init() {
    // https://github.com/spf13/viper/issues/461
    viper.BindEnv("node-name", "NODENAME")

    rootCmd.Flags().SortFlags = false
    rootCmd.PersistentFlags().SortFlags = false

    rootCmd.Flags().BoolVar(&failOnInitErrorFlag, "fail-on-init-error", true, "fail the plugin if an error is encountered during initialization, otherwise block indefinitely")
    rootCmd.Flags().StringVar(&runtimeSocketFlag, "runtime-socket", "/var/lib/vgpu/vgpu.sock", "runtime socket")
    rootCmd.Flags().UintVar(&config.DeviceSplitCount, "device-split-count", 2, "the number for NVIDIA device split")
    rootCmd.Flags().Float64Var(&config.DeviceMemoryScaling, "device-memory-scaling", 1.0, "the ratio for NVIDIA device memory scaling")
    rootCmd.Flags().Float64Var(&config.DeviceCoresScaling, "device-cores-scaling", 1.0, "the ratio for NVIDIA device cores scaling")
    rootCmd.Flags().StringVar(&config.SchedulerEndpoint, "scheduler-endpoint", "127.0.0.1:9090", "scheduler extender endpoint")
    rootCmd.Flags().IntVar(&config.SchedulerTimeout, "scheduler-timeout", 10, "scheduler connection timeout")
    rootCmd.Flags().StringVar(&config.NodeName, "node-name", viper.GetString("node-name"), "node name")

    rootCmd.PersistentFlags().AddGoFlagSet(util.GlobalFlagSet())
}

func start() error {
    klog.Infof("Loading NVML")
    if err := nvml.Init(); err != nil {
        klog.Infof("Failed to initialize NVML: %v.", err)
        klog.Infof("If this is a GPU node, did you set the docker default runtime to `nvidia`?")
        klog.Infof("You can check the prerequisites at: https://github.com/NVIDIA/k8s-device-plugin#prerequisites")
        klog.Infof("You can learn how to set the runtime at: https://github.com/NVIDIA/k8s-device-plugin#quick-start")
        klog.Infof("If this is not a GPU node, you should set up a toleration or nodeSelector to only deploy this plugin on GPU nodes")
        if failOnInitErrorFlag {
            return fmt.Errorf("failed to initialize NVML: %v", err)
        }
        select {}
    }
    defer func() { log.Println("Shutdown of NVML returned:", nvml.Shutdown()) }()

    log.Println("Starting FS watcher.")
    watcher, err := NewFSWatcher(pluginapi.DevicePluginPath)
    if err != nil {
        return fmt.Errorf("failed to create FS watcher: %v", err)
    }
    defer watcher.Close()

    log.Println("Starting OS watcher.")
    sigs := NewOSWatcher(syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

    cache := device_plugin.NewDeviceCache()
    cache.Start()
    defer cache.Stop()
    register := device_plugin.NewDeviceRegister(cache)
    register.Start()
    defer register.Stop()
    rt := device_plugin.NewVGPURuntimeService(cache)

    // start runtime grpc server
    lisGrpc, _ := net.Listen("unix", runtimeSocketFlag)
    defer lisGrpc.Close()
    s := grpc.NewServer()
    api.RegisterVGPURuntimeServiceServer(s, rt)
    go func() {
        err := s.Serve(lisGrpc)
        if err != nil {
            klog.Fatal(err)
        }
    }()

    var plugins []*device_plugin.NvidiaDevicePlugin
restart:
    // If we are restarting, idempotently stop any running plugins before
    // recreating them below.
    for _, p := range plugins {
        p.Stop()
    }

    log.Println("Retreiving plugins.")
    plugins = []*device_plugin.NvidiaDevicePlugin{
        device_plugin.NewNvidiaDevicePlugin(
            util.ResourceName,
            cache,
            gpuallocator.NewBestEffortPolicy(),
            pluginapi.DevicePluginPath+"nvidia-gpu.sock"),
    }

    // Loop through all plugins, starting them if they have any devices
    // to serve. If even one plugin fails to start properly, try
    // starting them all again.
    started := 0
    pluginStartError := make(chan struct{})
    for _, p := range plugins {
        // Just continue if there are no devices to serve for plugin p.
        if len(p.Devices()) == 0 {
            continue
        }

        // Start the gRPC server for plugin p and connect it with the kubelet.
        if err := p.Start(); err != nil {
            log.SetOutput(os.Stderr)
            log.Println("Could not contact Kubelet, retrying. Did you enable the device plugin feature gate?")
            log.Printf("You can check the prerequisites at: https://github.com/NVIDIA/k8s-device-plugin#prerequisites")
            log.Printf("You can learn how to set the runtime at: https://github.com/NVIDIA/k8s-device-plugin#quick-start")
            close(pluginStartError)
            goto events
        }
        started++
    }

    if started == 0 {
        log.Println("No devices found. Waiting indefinitely.")
    }

events:
    // Start an infinite loop, waiting for several indicators to either log
    // some messages, trigger a restart of the plugins, or exit the program.
    for {
        select {
        // If there was an error starting any plugins, restart them all.
        case <-pluginStartError:
            goto restart

        // Detect a kubelet restart by watching for a newly created
        // 'pluginapi.KubeletSocket' file. When this occurs, restart this loop,
        // restarting all of the plugins in the process.
        case event := <-watcher.Events:
            if event.Name == pluginapi.KubeletSocket && event.Op&fsnotify.Create == fsnotify.Create {
                log.Printf("inotify: %s created, restarting.", pluginapi.KubeletSocket)
                goto restart
            }

        // Watch for any other fs errors and log them.
        case err := <-watcher.Errors:
            log.Printf("inotify: %s", err)

        // Watch for any signals from the OS. On SIGHUP, restart this loop,
        // restarting all of the plugins in the process. On all other
        // signals, exit the loop and exit the program.
        case s := <-sigs:
            switch s {
            case syscall.SIGHUP:
                log.Println("Received SIGHUP, restarting.")
                goto restart
            default:
                log.Printf("Received signal \"%v\", shutting down.", s)
                for _, p := range plugins {
                    p.Stop()
                }
                break events
            }
        }
    }
    return nil
}

func main() {
    if err := rootCmd.Execute(); err != nil {
        klog.Fatal(err)
    }
}
