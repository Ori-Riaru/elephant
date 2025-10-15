flake: {
  config,
  lib,
  pkgs,
  ...
}:
with lib; let
  cfg = config.services.elephant;
  # Available providers
  providerOptions = {
    desktopapplications = "Desktop application launcher";
    files = "File search and management";
    clipboard = "Clipboard history management";
    runner = "Command runner";
    symbols = "Symbols and emojis";
    calc = "Calculator and unit conversion";
    menus = "Custom menu system";
    providerlist = "Provider listing and management";
    websearch = "Web search integration";
    todo = "Todo list";
    unicode = "Unicode symbol search";
    bluetooth = "Basic Bluetooth management";
  };
in {
  imports = [
    (mkRemovedOptionModule ["services" "elephant" "user"]
      "Elephant now runs as a user service and uses the current user automatically.")
    (mkRemovedOptionModule ["services" "elephant" "group"]
      "Elephant now runs as a user service and uses the current user's group automatically.")
  ];

  options.services.elephant = {
    enable = mkEnableOption "Elephant launcher backend user service";

    package = mkOption {
      type = types.package;
      default = flake.packages.${pkgs.stdenv.system}.elephant-with-providers;
      defaultText = literalExpression "flake.packages.\${pkgs.stdenv.system}.elephant-with-providers";
      description = "The elephant package to use.";
    };

    providers = mkOption {
      type = types.listOf (types.enum (attrNames providerOptions));
      default = attrNames providerOptions;
      example = [
        "files"
        "desktopapplications"
        "calc"
      ];
      description = ''
        List of providers to enable. Available providers:
        ${concatStringsSep "\n" (mapAttrsToList (name: desc: "  - ${name}: ${desc}") providerOptions)}
      '';
    };

    installService = mkOption {
      type = types.bool;
      default = true;
      description = "Create a systemd user service for elephant.";
    };

    debug = mkOption {
      type = types.bool;
      default = false;
      description = "Enable debug logging for elephant service.";
    };

    config = mkOption {
      type = types.attrs;
      default = {};
      example = literalExpression ''
        {
          providers = {
            files = {
              min_score = 50;
            };
            desktopapplications = {
              launch_prefix = "uwsm app --";
            };
          };
        }
      '';
      description = "Elephant configuration as Nix attributes.";
    };
  };

  config = mkIf cfg.enable {
    environment.systemPackages = [cfg.package];

    environment.etc =
      # Generate elephant config
      {
        "xdg/elephant/elephant.toml" = mkIf (cfg.config != {}) {
          source = (pkgs.formats.toml {}).generate "elephant.toml" cfg.config;
        };
      }
      # Generate provider files
      // builtins.listToAttrs
      (map
        (
          provider:
            lib.nameValuePair
            "xdg/elephant/providers/${provider}.so"
            {
              source = "${cfg.package}/lib/elephant/providers/${provider}.so";
            }
        )
        cfg.providers);

    systemd.user.services.elephant = mkIf cfg.installService {
      description = "Elephant launcher backend";
      after = ["graphical-session.target"];
      partOf = ["graphical-session.target"];
      wantedBy = ["graphical-session.target"];

      unitConfig = {
        ConditionEnvironment = "WAYLAND_DISPLAY";
      };

      serviceConfig = {
        Type = "simple";
        ExecStart = "${cfg.package}/bin/elephant ${optionalString cfg.debug "--debug"}";
        Restart = "on-failure";
        RestartSec = 1;

        # Clean up socket on stop
        ExecStopPost = "${pkgs.coreutils}/bin/rm -f /tmp/elephant.sock";
      };
    };
  };
}
